import ContainerAPIClient
import ContainerResource
import Containerization
import CryptoKit
import Foundation
import Darwin

// No installer, daemon, pull, global kernel setter or image garbage collector.
@main struct ToolDiskAdapter {
    static let commit = "a15b46097aa4c2325da13c1fb5192b9274aab083"
    struct Artifact: Codable { let path: String; let sha256: String }
    struct Manifest: Codable {
        let schema: Int
        let runtime_commit: String
        let format: String
        let adapter: Artifact
        let tools: Artifact
        let root: Artifact
        let kernel: Artifact
        let tool_sha256: String
        let image_reference: String
        let image_digest: String
        let init_reference: String
        let init_digest: String
        let review: Artifact
    }
    struct Mount: Codable { let path: String; let destination: String; let read_only: Bool }
    struct Request: Codable {
        let name: String
        let owner: String
        let directory: String
        let manifest: Manifest
        let profile: String
        let runtime: String
        let script: String
        let environment: [String]?
        let mounts: [Mount]?
        let network: String
        let workdir: String
        let cpus: Int
        let memory_gb: Int
    }
    static func require(_ value: Bool, _ message: String) throws {
        if !value { throw NSError(domain: "ToolDiskAdapter", code: 1, userInfo: [NSLocalizedDescriptionKey: message]) }
    }
    static func checked(_ path: String, directory: Bool = false) throws {
        let url = URL(fileURLWithPath: path).standardizedFileURL
        try require(url.path == path && url.resolvingSymlinksInPath() == url, "Noncanonical or symlink path")
        let attrs = try FileManager.default.attributesOfItem(atPath: path)
        try require(attrs[.type] as? FileAttributeType == (directory ? .typeDirectory : .typeRegular), "Unexpected file type")
        try require((attrs[.ownerAccountID] as? NSNumber)?.uint32Value == getuid(), "Wrong owner")
        try require(((attrs[.posixPermissions] as? NSNumber)?.intValue ?? 0o777) & 0o077 == 0, "Private permissions required")
        if !directory { try require((attrs[.referenceCount] as? NSNumber)?.intValue == 1, "Hardlink aliases rejected") }
    }
    static func verify(_ artifact: Artifact, privateFile: Bool = true) throws {
        if privateFile { try checked(artifact.path) }
        try require(artifact.sha256.count == 64 && artifact.sha256.allSatisfy { $0.isHexDigit }, "Invalid SHA256")
        let f = try FileHandle(forReadingFrom: URL(fileURLWithPath: artifact.path))
        defer { try? f.close() }
        var hash = SHA256()
        while let data = try f.read(upToCount: 1024 * 1024), !data.isEmpty { hash.update(data: data) }
        try require(hash.finalize().map { String(format: "%02x", $0) }.joined() == artifact.sha256, "Artifact digest mismatch")
    }
    static func run(_ executable: String, _ arguments: [String]) throws -> Data {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: executable)
        process.arguments = arguments
        process.environment = ["PATH": "/usr/bin:/bin:/usr/sbin:/sbin"]
        let output = Pipe()
        process.standardOutput = output
        process.standardError = FileHandle.standardError
        process.standardInput = FileHandle.nullDevice
        try process.run()
        let data = output.fileHandleForReading.readDataToEndOfFile()
        process.waitUntilExit()
        try require(process.terminationStatus == 0, "Local command failed: \(executable)")
        return data
    }
    // Match all three: backing image, private mountpoint and readonly owner uid.
    static func mounted(_ r: Request) throws -> Bool {
        let data = try run("/usr/bin/hdiutil", ["info", "-plist"])
        let plist = try PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any]
        let point = r.directory + "/volume"
        for image in plist?["images"] as? [[String: Any]] ?? [] {
            for entity in image["system-entities"] as? [[String: Any]] ?? [] where entity["mount-point"] as? String == point {
                try require(image["image-path"] as? String == r.manifest.tools.path, "Mount backing image differs")
                try require((image["owner-uid"] as? NSNumber)?.uint32Value == getuid(), "Mount owner differs")
                try require((image["writeable"] as? Bool) == false, "Mount is not readonly")
                try require(image["image-type"] as? String == "UDIF read-only compressed (lzfse)", "Expected an LZFSE/ULFO volume")
                return true
            }
        }
        return false
    }
    static func toolPath(_ r: Request, allowAttach: Bool) throws -> String {
        try verify(r.manifest.tools)
        if r.manifest.format == "erofs" {
            try require(r.manifest.tools.sha256 == r.manifest.tool_sha256, "Raw disk hash differs")
            return r.manifest.tools.path
        }
        try require(r.manifest.format == "ulfo", "Unsupported tool disk format")
        let point = r.directory + "/volume"
        if !(try mounted(r)) {
            try require(allowAttach, "Owned tool volume is not attached")
            if !FileManager.default.fileExists(atPath: point) {
                try FileManager.default.createDirectory(atPath: point, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
            }
            try checked(point, directory: true)
            try require(try FileManager.default.contentsOfDirectory(atPath: point).isEmpty, "Mountpoint is not empty")
            _ = try run("/usr/bin/hdiutil", ["attach", "-readonly", "-nobrowse", "-owners", "on", "-mountpoint", point, r.manifest.tools.path])
            try require(try mounted(r), "Could not verify attached volume")
        }
        let path = point + "/tools.erofs"
        let url = URL(fileURLWithPath: path)
        try require(url.resolvingSymlinksInPath() == url, "Tool disk symlink rejected")
        let attrs = try FileManager.default.attributesOfItem(atPath: path)
        try require(attrs[.type] as? FileAttributeType == .typeRegular, "Tool disk is not regular")
        try verify(.init(path: path, sha256: r.manifest.tool_sha256), privateFile: false)
        return path
    }
    static func main() async {
        do { try await mainThrowing() }
        catch { FileHandle.standardError.write(Data("ToolDiskAdapter: \(error)\n".utf8)); exit(1) }
    }
    static func mainThrowing() async throws {
        try require(CommandLine.arguments.count == 2, "usage: ToolDiskAdapter probe|create|check|remove (JSON on stdin)")
        let op = CommandLine.arguments[1]
        try require(["probe", "create", "check", "remove"].contains(op), "Unknown operation")
        let health = try await ClientHealthCheck.ping(timeout: .seconds(10))
        try require(health.apiServerCommit == commit, "Requires pinned API server 1.7.1 at \(commit); installed daemon is \(health.apiServerVersion) at \(health.apiServerCommit). Global runtime was not changed.")
        #if !arch(arm64)
        throw NSError(domain: "ToolDiskAdapter", code: 2, userInfo: [NSLocalizedDescriptionKey: "ARM64 host required"])
        #endif
        if op == "probe" { return }
        let data = FileHandle.standardInput.readDataToEndOfFile()
        try require(data.count <= 262144, "Request too large")
        let r = try JSONDecoder().decode(Request.self, from: data)
        try require(r.name.range(of: "^saint-[a-z0-9][a-z0-9-]{0,31}$", options: .regularExpression) != nil, "Invalid container name")
        try require(r.owner.count == 64 && r.owner.allSatisfy { $0.isHexDigit }, "Invalid owner nonce")
        try checked(r.directory, directory: true)
        try require(URL(fileURLWithPath: r.directory).lastPathComponent == r.name, "Ownership directory differs")
        try checked(r.directory + "/owner.json")
        let saved = try JSONDecoder().decode(Request.self, from: Data(contentsOf: URL(fileURLWithPath: r.directory + "/owner.json")))
        try require(saved.owner == r.owner && saved.profile == r.profile && saved.name == r.name, "Ownership record differs")
        let lock = open(r.directory + "/adapter.lock", O_CREAT | O_RDWR | O_NOFOLLOW, 0o600)
        try require(lock >= 0, "Could not open ownership lock")
        defer { close(lock) }
        try require(flock(lock, LOCK_EX | LOCK_NB) == 0, "Another lifecycle operation owns this root")
        let client = ContainerClient()
        let existing = try await client.list(filters: .init(ids: [r.name]))
        for vm in existing {
            try require(vm.configuration.labels["saint.profile.owner"] == r.owner && vm.configuration.labels["saint.profile.manifest"] == r.profile, "VM ownership differs; retained all resources")
        }
        let root = r.directory + "/root.ext4"
        if op == "remove" {
            for vm in existing {
                if vm.status == .running { try await client.stop(id: r.name) }
                try await client.delete(id: r.name, force: false)
            }
            try require(try await client.list(filters: .init(ids: [r.name])).isEmpty, "VM still exists; retained disks")
            if try r.manifest.format == "ulfo" && mounted(r) {
                _ = try run("/usr/bin/hdiutil", ["detach", r.directory + "/volume"])
                try require(!(try mounted(r)), "Mount remains; retained root")
            }
            // Never recursively delete or prune shared image stores.
            if FileManager.default.fileExists(atPath: root) { try checked(root); try FileManager.default.removeItem(atPath: root) }
            let point = r.directory + "/volume"
            if FileManager.default.fileExists(atPath: point) {
                try checked(point, directory: true)
                try require(try FileManager.default.contentsOfDirectory(atPath: point).isEmpty, "Mountpoint not empty; retained")
                try require(rmdir(point) == 0, "Could not remove empty mountpoint")
            }
            return
        }
        if op == "check" {
            try require(existing.count == 1, "Owned VM absent")
            try checked(root)
            let path = try toolPath(r, allowAttach: existing[0].status != .running)
            try require(existing[0].configuration.mounts.contains { $0.source == path && $0.destination == "/usr" && $0.options.contains("ro") }, "Readonly tool attachment differs")
            return
        }
        try require(existing.isEmpty, "Refusing to adopt an existing VM")
        try require(r.manifest.runtime_commit == commit && r.manifest.schema == 1, "Unsupported manifest")
        try require(r.network == r.name && r.cpus > 0 && r.cpus <= 64 && r.memory_gb > 0 && r.memory_gb <= 128, "Invalid resources or network")
        try require(!FileManager.default.fileExists(atPath: root), "Writable root already exists; reconcile pending creation first")
        for artifact in [r.manifest.root, r.manifest.kernel, r.manifest.review] { try verify(artifact) }
        let images = try await ClientImage.list()
        guard let image = images.first(where: { $0.reference == r.manifest.image_reference && $0.digest == r.manifest.image_digest }) else {
            throw NSError(domain: "ToolDiskAdapter", code: 3, userInfo: [NSLocalizedDescriptionKey: "Pinned scaffold image not present; no implicit pull"])
        }
        try require(images.contains { $0.reference == r.manifest.init_reference && $0.digest == r.manifest.init_digest }, "Pinned init image not present; no implicit pull")
        try FileManager.default.copyItem(atPath: r.manifest.root.path, toPath: root)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: root)
        try checked(root)
        try verify(.init(path: root, sha256: r.manifest.root.sha256))
        let rootHandle = try FileHandle(forWritingTo: URL(fileURLWithPath: root))
        try rootHandle.synchronize()
        try rootHandle.close()
        let tool = try toolPath(r, allowAttach: true)
        let env = ["PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/home/agent", "LANG=C.UTF-8", "ENABLE_CLAUDEAI_MCP_SERVERS=false"] + (r.environment ?? [])
        let process = ProcessConfiguration(executable: "/bin/sh", arguments: ["-lc", r.script], environment: env, workingDirectory: r.workdir, user: .id(uid: 1000, gid: 1000))
        var config = ContainerConfiguration(id: r.name, image: image.description, process: process)
        config.labels = ["saint.runtime": r.runtime, "saint.profile.owner": r.owner, "saint.profile.manifest": r.profile]
        config.networks = [.init(network: r.network, options: .init(hostname: r.name))]
        config.ssh = false
        config.useInit = true
        config.resources.cpus = r.cpus
        config.resources.memoryInBytes = UInt64(r.memory_gb) * 1024 * 1024 * 1024
        config.mounts = [.block(format: "erofs", source: tool, destination: "/usr", options: ["ro"])]
        for mount in r.mounts ?? [] { config.mounts.append(.virtiofs(source: mount.path, destination: mount.destination, options: mount.read_only ? ["ro"] : [])) }
        let kernel = Kernel(path: URL(fileURLWithPath: r.manifest.kernel.path), platform: .linuxArm)
        try await client.create(configuration: config, options: .init(autoRemove: false, rootFsOverride: .block(format: "ext4", source: root, destination: "/", options: [])), kernel: kernel, initImage: r.manifest.init_reference)
    }
}

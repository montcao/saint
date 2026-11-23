import ContainerAPIClient
import ContainerResource
import Containerization
import CryptoKit
import Foundation
import Darwin

// No installer, daemon, pull, global kernel setter or image garbage collector.
@main struct ToolDiskAdapter {
    static let commit = "a15b46097aa4c2325da13c1fb5192b9274aab083"
    struct Artifact: Codable, Equatable { let path: String; let sha256: String }
    struct Manifest: Codable, Equatable {
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
    struct Mount: Codable, Equatable { let path: String; let destination: String; let read_only: Bool }
    struct Request: Codable, Equatable {
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
    static func canonical(_ path: String) throws {
        try require(path.hasPrefix("/") && !path.utf8.contains(0), "Noncanonical path")
        guard let resolved = realpath(path, nil) else {
            throw NSError(domain: "ToolDiskAdapter", code: 1, userInfo: [NSLocalizedDescriptionKey: "Path resolution failed"])
        }
        defer { free(resolved) }
        try require(String(cString: resolved) == path, "Symlink path rejected")
    }
    static func checked(_ path: String, directory: Bool = false) throws {
        try canonical(path)
        let attrs = try FileManager.default.attributesOfItem(atPath: path)
        try require(attrs[.type] as? FileAttributeType == (directory ? .typeDirectory : .typeRegular), "Unexpected file type")
        try require((attrs[.ownerAccountID] as? NSNumber)?.uint32Value == getuid(), "Wrong owner")
        try require(((attrs[.posixPermissions] as? NSNumber)?.intValue ?? 0o777) & 0o077 == 0, "Private permissions required")
        if !directory { try require((attrs[.referenceCount] as? NSNumber)?.intValue == 1, "Hardlink aliases rejected") }
        var ancestor = path
        while ancestor != "/" {
            let slash = ancestor.lastIndex(of: "/")!
            ancestor = slash == ancestor.startIndex ? "/" : String(ancestor[..<slash])
            var info = stat()
            try require(lstat(ancestor, &info) == 0 && (info.st_mode & S_IFMT) == S_IFDIR, "Unsafe ancestor type")
            try require(info.st_uid == 0 || info.st_uid == getuid(), "Unsafe ancestor owner")
            try require(info.st_mode & 0o022 == 0 || info.st_mode & S_ISVTX != 0, "Writable ancestor")
        }
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
        try canonical(path)
        let attrs = try FileManager.default.attributesOfItem(atPath: path)
        try require(attrs[.type] as? FileAttributeType == .typeRegular, "Tool disk is not regular")
        try verify(.init(path: path, sha256: r.manifest.tool_sha256), privateFile: false)
        return path
    }
    static func readBounded(_ file: FileHandle, limit: Int) throws -> Data {
        var data = Data()
        while let part = try file.read(upToCount: min(65536, limit + 1 - data.count)), !part.isEmpty {
            data.append(part)
            try require(data.count <= limit, "Input exceeds limit")
        }
        return data
    }
    struct RootIdentity: Codable, Equatable { let device: UInt64; let inode: UInt64 }
    static func rootIdentity(_ r: Request) throws -> RootIdentity {
        let root = r.directory + "/root.ext4"
        try checked(root)
        let attrs = try FileManager.default.attributesOfItem(atPath: root)
        guard let device = attrs[.systemNumber] as? NSNumber, let inode = attrs[.systemFileNumber] as? NSNumber else {
            throw NSError(domain: "ToolDiskAdapter", code: 1, userInfo: [NSLocalizedDescriptionKey: "Root identity unavailable"])
        }
        return RootIdentity(device: device.uint64Value, inode: inode.uint64Value)
    }
    static func saveRootIdentity(_ r: Request) throws {
        let data = try JSONEncoder().encode(rootIdentity(r))
        let fd = open(r.directory + "/root-identity.json", O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW, 0o600)
        try require(fd >= 0, "Could not reserve root identity")
        let file = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
        try file.write(contentsOf: data)
        try file.synchronize()
        try file.close()
        let parent = open(r.directory, O_RDONLY | O_DIRECTORY | O_NOFOLLOW)
        try require(parent >= 0, "Could not open ownership directory")
        defer { close(parent) }
        try require(fsync(parent) == 0, "Could not persist root identity")
    }
    static func verifyRootIdentity(_ r: Request) throws {
        let path = r.directory + "/root-identity.json"
        try checked(path)
        let file = try FileHandle(forReadingFrom: URL(fileURLWithPath: path))
        defer { try? file.close() }
        let saved = try JSONDecoder().decode(RootIdentity.self, from: readBounded(file, limit: 1024))
        try require(saved == rootIdentity(r), "Writable root identity differs; retained")
    }
    static func guestEnvironment(_ r: Request) -> [String] {
        ["PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/home/agent", "LANG=C.UTF-8", "TERM=xterm-256color", "ENABLE_CLAUDEAI_MCP_SERVERS=false"] + (r.environment ?? [])
    }
    static func validateNetwork(_ r: Request) async throws {
        try require(r.network == r.name, "Network identity differs")
        let network = try await NetworkClient().get(id: r.network)
        try require(network.configuration.mode == .hostOnly && network.configuration.labels["saint.profile.owner"] == r.owner, "Network isolation or ownership differs")
    }
    static func validateConfiguration(_ c: ContainerConfiguration, request r: Request, tool: String) throws {
        try require(c.networks.count == 1 && c.networks[0].network == r.network, "Network attachments differ")
        try require(!c.ssh && !c.virtualization && !c.rosetta && c.useInit && c.publishedPorts.isEmpty && c.publishedSockets.isEmpty && c.capAdd.isEmpty, "Isolation configuration differs")
        let process = c.initProcess
        try require(process.executable == "/bin/sh" && process.arguments == ["-c", r.script] && process.environment == guestEnvironment(r) && process.workingDirectory == r.workdir && process.terminal && process.user == .id(uid: 1000, gid: 1000), "Guest process configuration differs")
        var expected = [Filesystem.block(format: "erofs", source: tool, destination: "/usr", options: ["ro"])]
        for mount in r.mounts ?? [] { expected.append(.virtiofs(source: mount.path, destination: mount.destination, options: mount.read_only ? ["ro"] : [])) }
        try require(c.mounts.count == expected.count, "Unexpected guest mounts")
        for (actual, wanted) in zip(c.mounts, expected) {
            try require(actual.type == wanted.type && actual.source == wanted.source && actual.destination == wanted.destination && actual.options == wanted.options, "Guest mount differs")
        }
        // The snapshot API does not expose rootFsOverride. Its binding must also
        // pass live qualification; the private inode ledger detects replacement.
    }
    static func main() async {
        do { try await mainThrowing() }
        catch { FileHandle.standardError.write(Data("ToolDiskAdapter: \(error)\n".utf8)); exit(1) }
    }
    static func mainThrowing() async throws {
        try require(CommandLine.arguments.count == 2, "usage: ToolDiskAdapter probe|create|check|stop|remove (JSON on stdin)")
        let op = CommandLine.arguments[1]
        try require(["probe", "create", "check", "stop", "remove"].contains(op), "Unknown operation")
        let health = try await ClientHealthCheck.ping(timeout: .seconds(10))
        try require(health.apiServerCommit == commit, "Requires pinned API server 1.7.1 at \(commit); installed daemon is \(health.apiServerVersion) at \(health.apiServerCommit). Global runtime was not changed.")
        #if !arch(arm64)
        throw NSError(domain: "ToolDiskAdapter", code: 2, userInfo: [NSLocalizedDescriptionKey: "ARM64 host required"])
        #endif
        if op == "probe" { return }
        let data = try readBounded(FileHandle.standardInput, limit: 262144)
        try require(data.count <= 262144, "Request too large")
        let r = try JSONDecoder().decode(Request.self, from: data)
        try require(r.name.range(of: "^saint-[a-z0-9][a-z0-9-]{0,31}$", options: .regularExpression) != nil, "Invalid container name")
        try require(r.owner.count == 64 && r.owner.allSatisfy { $0.isHexDigit }, "Invalid owner nonce")
        try checked(r.directory, directory: true)
        try require(URL(fileURLWithPath: r.directory).lastPathComponent == r.name, "Ownership directory differs")
        try checked(r.directory + "/owner.json")
        let ledger = try FileHandle(forReadingFrom: URL(fileURLWithPath: r.directory + "/owner.json"))
        defer { try? ledger.close() }
        let saved = try JSONDecoder().decode(Request.self, from: readBounded(ledger, limit: 262144))
        try require(saved == r, "Ownership record differs")
        let lock = open(r.directory + "/adapter.lock", O_CREAT | O_RDWR | O_NOFOLLOW, 0o600)
        try require(lock >= 0, "Could not open ownership lock")
        defer { close(lock) }
        var lockInfo = stat()
        try require(fstat(lock, &lockInfo) == 0 && (lockInfo.st_mode & S_IFMT) == S_IFREG && lockInfo.st_uid == getuid() && lockInfo.st_nlink == 1 && lockInfo.st_mode & 0o077 == 0, "Unsafe ownership lock")
        try require(flock(lock, LOCK_EX | LOCK_NB) == 0, "Another lifecycle operation owns this root")
        let client = ContainerClient()
        let existing = try await client.list(filters: .init(ids: [r.name]))
        for vm in existing {
            try require(vm.configuration.labels["saint.profile.owner"] == r.owner && vm.configuration.labels["saint.profile.manifest"] == r.profile, "VM ownership differs; retained all resources")
        }
        let root = r.directory + "/root.ext4"
        // Stopping must remain possible when a tool artifact has been damaged.
        // Ownership is required; artifact integrity is deliberately not required.
        if op == "stop" {
            for _ in existing { try await client.stop(id: r.name) }
            try require(!(try await client.list(filters: .init(ids: [r.name]))).contains { $0.status != .stopped }, "Owned VM has not stopped")
            return
        }
        if op == "remove" {
            for _ in existing {
                try await client.stop(id: r.name)
                try await client.delete(id: r.name, force: false)
            }
            try require(try await client.list(filters: .init(ids: [r.name])).isEmpty, "VM still exists; retained disks")
            if try r.manifest.format == "ulfo" && mounted(r) {
                _ = try run("/usr/bin/hdiutil", ["detach", r.directory + "/volume"])
                try require(!(try mounted(r)), "Mount remains; retained root")
            }
            // Never recursively delete or prune shared image stores.
            if FileManager.default.fileExists(atPath: root) { try verifyRootIdentity(r); try FileManager.default.removeItem(atPath: root) }
            let identity = r.directory + "/root-identity.json"
            if FileManager.default.fileExists(atPath: identity) { try checked(identity); try FileManager.default.removeItem(atPath: identity) }
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
            try require(existing[0].status == .stopped, "Owned VM is not stopped; refusing attachment")
            // The runtime reopens the configured kernel on start. Recheck its
            // immutable bytes as well as the readonly tool disk before each boot.
            for artifact in [r.manifest.kernel, r.manifest.review] { try verify(artifact) }
            try verifyRootIdentity(r)
            let path = try toolPath(r, allowAttach: true)
            try validateConfiguration(existing[0].configuration, request: r, tool: path)
            try await validateNetwork(r)
            return
        }
        try require(existing.isEmpty, "Refusing to adopt an existing VM")
        try require(r.manifest.runtime_commit == commit && r.manifest.schema == 1, "Unsupported manifest")
        try require(r.network == r.name && r.cpus > 0 && r.cpus <= 64 && r.memory_gb > 0 && r.memory_gb <= 128, "Invalid resources or network")
        try await validateNetwork(r)
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
        try saveRootIdentity(r)
        try verify(.init(path: root, sha256: r.manifest.root.sha256))
        let rootHandle = try FileHandle(forWritingTo: URL(fileURLWithPath: root))
        try rootHandle.synchronize()
        try rootHandle.close()
        let tool = try toolPath(r, allowAttach: true)
        let process = ProcessConfiguration(executable: "/bin/sh", arguments: ["-c", r.script], environment: guestEnvironment(r), workingDirectory: r.workdir, terminal: true, user: .id(uid: 1000, gid: 1000))
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

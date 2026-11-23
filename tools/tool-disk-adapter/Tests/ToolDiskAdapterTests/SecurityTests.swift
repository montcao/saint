import Testing
import Foundation
import Darwin
import ContainerResource
@testable import ToolDiskAdapter

struct SecurityTests {
    func request(_ directory: String = "/private/synthetic/saint-fixture") -> ToolDiskAdapter.Request {
        let artifact = ToolDiskAdapter.Artifact(path: "/private/synthetic/artifact", sha256: String(repeating: "1", count: 64))
        let digest = "sha256:" + String(repeating: "1", count: 64)
        let manifest = ToolDiskAdapter.Manifest(schema: 1, runtime_commit: ToolDiskAdapter.commit, format: "erofs", adapter: artifact, tools: artifact, root: artifact, kernel: artifact, tool_sha256: artifact.sha256, image_reference: "example.test/image@" + digest, image_digest: digest, init_reference: "example.test/init@" + digest, init_digest: digest, review: artifact)
        return ToolDiskAdapter.Request(name: "saint-fixture", owner: String(repeating: "2", count: 64), directory: directory, manifest: manifest, profile: "synthetic-profile", runtime: "claude", script: "exec synthetic-agent", environment: [], mounts: [], network: "saint-fixture", workdir: "/workspace", cpus: 2, memory_gb: 2)
    }
    func privateDirectory() throws -> URL {
        let base = try #require(realpath(FileManager.default.temporaryDirectory.path, nil))
        defer { free(base) }
        let directory = URL(fileURLWithPath: String(cString: base)).appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        return directory
    }
    func configuration(_ r: ToolDiskAdapter.Request) throws -> ContainerConfiguration {
        let bytes = Data(#"{"reference":"example.test/image","descriptor":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","size":1}}"#.utf8)
        let image = try JSONDecoder().decode(ImageDescription.self, from: bytes)
        let process = ProcessConfiguration(executable: "/bin/sh", arguments: ["-c", r.script], environment: ToolDiskAdapter.guestEnvironment(r), workingDirectory: r.workdir, terminal: true, user: .id(uid: 1000, gid: 1000))
        var c = ContainerConfiguration(id: r.name, image: image, process: process)
        c.networks = [.init(network: r.network, options: .init(hostname: r.name))]
        c.useInit = true
        c.mounts = [.block(format: "erofs", source: r.manifest.tools.path, destination: "/usr", options: ["ro"])]
        return c
    }
    @Test func testResumeRejectsWeakenedIsolation() throws {
        let r = request()
        let original = try configuration(r)
        #expect(throws: Never.self) { try ToolDiskAdapter.validateConfiguration(original, request: r, tool: r.manifest.tools.path) }
        let mutations: [(inout ContainerConfiguration) -> Void] = [
            { $0.ssh = true },
            { $0.virtualization = true },
            { $0.mounts[0].options = [] },
            { $0.mounts.append(.virtiofs(source: "/private/synthetic/unreviewed", destination: "/extra", options: [])) },
            { $0.networks = [.init(network: "default", options: .init(hostname: r.name))] },
            { $0.initProcess.environment.append("CLAUDE_CODE_OAUTH_TOKEN=synthetic-only") },
            { $0.initProcess.terminal = false },
            { $0.initProcess.user = .id(uid: 0, gid: 0) },
            { $0.initProcess.arguments = ["-lc", r.script] },
        ]
        for mutate in mutations {
            var changed = original
            mutate(&changed)
            #expect(throws: (any Error).self) { try ToolDiskAdapter.validateConfiguration(changed, request: r, tool: r.manifest.tools.path) }
        }
    }
    @Test func testWritableRootReplacementAndAliasesAreRejected() throws {
        let directory = try privateDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let root = directory.appendingPathComponent("root.ext4")
        try Data("synthetic writable root".utf8).write(to: root)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: root.path)
        let r = request(directory.path)
        try ToolDiskAdapter.saveRootIdentity(r)
        #expect(throws: Never.self) { try ToolDiskAdapter.verifyRootIdentity(r) }
        let alias = directory.appendingPathComponent("alias")
        try FileManager.default.linkItem(at: root, to: alias)
        #expect(throws: (any Error).self) { try ToolDiskAdapter.verifyRootIdentity(r) }
        try FileManager.default.removeItem(at: alias)
        try FileManager.default.moveItem(at: root, to: directory.appendingPathComponent("original"))
        try Data("replacement".utf8).write(to: root)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: root.path)
        #expect(throws: (any Error).self) { try ToolDiskAdapter.verifyRootIdentity(r) }
    }
    @Test func testBoundedReaderRejectsOversizedInput() throws {
        let directory = try privateDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let path = directory.appendingPathComponent("input")
        try Data(repeating: 65, count: 1025).write(to: path)
        let file = try FileHandle(forReadingFrom: path)
        defer { try? file.close() }
        #expect(throws: (any Error).self) { try ToolDiskAdapter.readBounded(file, limit: 1024) }
    }
}

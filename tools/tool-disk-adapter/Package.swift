// swift-tools-version: 6.2
import PackageDescription
let package = Package(
    name: "ToolDiskAdapter",
    platforms: [.macOS("15")],
    dependencies: [
        .package(url: "https://github.com/apple/container.git", revision: "a15b46097aa4c2325da13c1fb5192b9274aab083"),
        .package(url: "https://github.com/apple/containerization.git", exact: "0.54.0"),
    ],
    targets: [.executableTarget(name: "ToolDiskAdapter", dependencies: [
        .product(name: "ContainerAPIClient", package: "container"),
        .product(name: "ContainerResource", package: "container"),
        .product(name: "Containerization", package: "containerization"),
    ])]
)

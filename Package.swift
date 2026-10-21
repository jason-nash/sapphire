// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "Sapphire",
    platforms: [.macOS(.v15)],
    products: [
        .library(name: "SapphireCore", targets: ["SapphireCore"]),
        .executable(name: "sapphire", targets: ["SapphireCLI"]),
        .executable(name: "SapphireDesktop", targets: ["SapphireApp"]),
    ],
    targets: [
        .target(name: "SapphireCore"),
        .executableTarget(name: "SapphireCLI", dependencies: ["SapphireCore"]),
        .executableTarget(name: "SapphireApp", dependencies: ["SapphireCore"]),
        .testTarget(name: "SapphireCoreTests", dependencies: ["SapphireCore"]),
        .testTarget(name: "SapphireAppTests", dependencies: ["SapphireApp", "SapphireCore"]),
    ]
)

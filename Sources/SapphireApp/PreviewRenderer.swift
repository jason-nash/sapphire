import AppKit
import SapphireCore
import SwiftUI

@MainActor
enum PreviewRenderer {
    static func render(to path: String) throws {
        let url = try destination(path)
        // This branch never starts SwiftUI's App lifecycle or creates an NSWindow.
        NSApplication.shared.setActivationPolicy(.prohibited)
        let model = makeModel()
        let view = WorkspaceView(model: model)
            .frame(width: 1280, height: 820)
            .environment(\.colorScheme, .light)
        let hosting = NSHostingView(rootView: view)
        hosting.appearance = NSAppearance(named: .aqua)
        hosting.frame = NSRect(x: 0, y: 0, width: 1280, height: 820)
        hosting.layoutSubtreeIfNeeded()
        guard let bitmap = hosting.bitmapImageRepForCachingDisplay(in: hosting.bounds) else {
            throw SapphireError.unavailable("Unable to allocate an offscreen view bitmap.")
        }
        hosting.cacheDisplay(in: hosting.bounds, to: bitmap)
        guard let png = bitmap.representation(using: .png, properties: [:]) else {
            throw SapphireError.unavailable("Unable to encode the offscreen preview.")
        }
        try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        try png.write(to: url, options: .atomic)
    }

    static func renderIcon(to path: String) throws {
        let url = try destination(path)
        NSApplication.shared.setActivationPolicy(.prohibited)
        let renderer = ImageRenderer(content: SapphireMark(size: 824).padding(100).frame(width: 1024, height: 1024))
        renderer.scale = 1
        guard let image = renderer.cgImage,
              let data = NSBitmapImageRep(cgImage: image).representation(using: .png, properties: [:]) else {
            throw SapphireError.unavailable("Unable to render the app icon.")
        }
        try data.write(to: url, options: .atomic)
    }

    private static func destination(_ path: String) throws -> URL {
        guard path.hasPrefix("/"), path.lowercased().hasSuffix(".png") else {
            throw SapphireError.invalidInput("The render destination must be an absolute path ending in .png.")
        }
        return URL(fileURLWithPath: path)
    }

    // Illustrative data is constructed only after --render-preview is selected.
    static func makeModel() -> SapphireModel {
        let model = SapphireModel(isPreview: true)
        var snapshot = RuntimeSnapshot()
        snapshot.virtualMachine = VMStatus(running: true, cpus: 2, memoryMiB: 1024)
        snapshot.engineRunning = true
        snapshot.containers = [
            DockerContainer(
                id: "preview-web", name: "sapphire-web", image: "nginx:alpine",
                state: "running", status: "Up 12 minutes", ports: "127.0.0.1:8080 -> 80/tcp", project: "sapphire"
            ),
            DockerContainer(
                id: "preview-api", name: "sapphire-api", image: "node:22-alpine",
                state: "running", status: "Up 12 minutes", ports: "127.0.0.1:3000 -> 3000/tcp", project: "sapphire"
            ),
            DockerContainer(
                id: "preview-db", name: "postgres", image: "postgres:17-alpine",
                state: "running", status: "Up 12 minutes", ports: "5432/tcp", project: "sapphire"
            ),
            DockerContainer(
                id: "preview-cache", name: "redis", image: "redis:7-alpine",
                state: "running", status: "Up 12 minutes", ports: "6379/tcp", project: "sapphire"
            ),
            DockerContainer(
                id: "preview-worker", name: "background-worker", image: "python:3.13-alpine",
                state: "exited", status: "Exited (0)"
            ),
        ]
        snapshot.images = [
            DockerImage(id: "preview-image-web", repository: "nginx", tag: "alpine", size: "48 MB", created: "2 days ago"),
            DockerImage(id: "preview-image-api", repository: "node", tag: "22-alpine", size: "156 MB", created: "3 days ago"),
            DockerImage(id: "preview-image-db", repository: "postgres", tag: "17-alpine", size: "274 MB", created: "4 days ago"),
        ]
        snapshot.volumes = [.init(name: "postgres-data", driver: "local"), .init(name: "redis-data", driver: "local")]
        snapshot.networks = [.init(id: "preview-network", name: "sapphire_default", driver: "bridge", scope: "local")]
        snapshot.projects = [.init(name: "sapphire", status: "running(4)", configFiles: "/workspace/sapphire/compose.yaml")]
        snapshot.machines = [.init(
            name: "workspace", status: "Running", cpus: 2, memory: 1_073_741_824,
            dir: "/var/lib/sapphire/machines/workspace"
        )]
        model.snapshot = snapshot
        model.hasLoaded = true
        model.dependencies = DependencyStatus(engine: "preview-only", docker: "preview-only", guestAssets: true)
        model.selectedID = ResourceRecord.container(snapshot.containers[0]).id
        return model
    }

    static func detail(for record: ResourceRecord, tab: DetailTab) -> String {
        switch tab {
        case .overview: return ""
        case .logs: return "[Illustrative preview only]\nNo production runtime was queried."
        case .inspect: return "{\n  \"preview\": true,\n  \"name\": \"\(record.name)\"\n}"
        case .shell: return "# Illustrative preview only. No terminal command was generated."
        }
    }
}

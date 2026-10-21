import AppKit
import SwiftUI

@main
enum SapphireEntry {
    @MainActor
    static func main() {
        let arguments = Array(CommandLine.arguments.dropFirst())
        if arguments.first == "--render-preview" || arguments.first == "--render-icon" {
            do {
                guard arguments.count == 2 else {
                    throw RenderArgumentError()
                }
                if arguments[0] == "--render-preview" {
                    try PreviewRenderer.render(to: arguments[1])
                } else {
                    try PreviewRenderer.renderIcon(to: arguments[1])
                }
                return
            } catch {
                FileHandle.standardError.write(Data("Sapphire: \(error.localizedDescription)\n".utf8))
                exit(1)
            }
        }
        guard arguments.isEmpty else {
            FileHandle.standardError.write(Data("Usage: Sapphire [--render-preview /absolute/path.png]\n".utf8))
            exit(2)
        }
        SapphireDesktopApp.main()
    }
}

private struct RenderArgumentError: LocalizedError {
    var errorDescription: String? { "Provide exactly one absolute PNG destination for the render option." }
}

struct SapphireDesktopApp: App {
    @StateObject private var model = SapphireModel()

    var body: some Scene {
        Window("Sapphire", id: "workspace") {
            WorkspaceView(model: model)
                .background(SapphireTheme.surface)
        }
        .defaultSize(width: 1280, height: 820)
        .windowStyle(.hiddenTitleBar)
        .windowResizability(.contentMinSize)
        .commands {
            CommandGroup(replacing: .appInfo) {
                Button("About Sapphire") { model.showAbout = true }
            }
            CommandGroup(replacing: .newItem) {
                Button("Refresh Workspace") { model.activate() }
                    .keyboardShortcut("r", modifiers: .command)
                    .disabled(!model.canAct)
                Button(model.section.addTitle + "...") { model.presentCreationForm() }
                    .keyboardShortcut("n", modifiers: .command)
                    .disabled(!model.canAct || !creationAvailable)
            }
            CommandMenu("Runtime") {
                Button("Configure & Start Shared VM...") { model.presentedForm = .startVM }
                    .disabled(!model.canAct || model.snapshot.virtualMachine.running || !model.canStartVM)
                Button("Stop Shared VM...") { model.requestStopVM() }
                    .disabled(!model.canAct || !model.snapshot.virtualMachine.running)
                Divider()
                Button("Start Container Engine") { model.startDocker() }
                    .disabled(!model.canAct || model.snapshot.engineRunning || !model.canStartDocker)
                Button("Stop Container Engine...") { model.requestStopDocker() }
                    .disabled(!model.canAct || !model.snapshot.engineRunning)
            }
            CommandMenu("Workspace") {
                ForEach(Array(WorkspaceSection.allCases.enumerated()), id: \.element.id) { index, section in
                    Button(section.title) { model.navigate(to: section) }
                        .keyboardShortcut(KeyEquivalent(Character("\(index + 1)")), modifiers: .command)
                }
            }
        }
    }

    private var creationAvailable: Bool {
        switch model.section {
        case .machines: model.canStartVM
        case .kubernetes: model.canStartDocker
        default: model.snapshot.engineRunning
        }
    }
}

import Foundation
import SapphireCore
import Testing
@testable import SapphireApp

private actor ModelRunner: CommandRunning {
    private var responses: [String]
    private(set) var commands: [RuntimeCommand] = []
    private(set) var cancellations = 0

    init(_ responses: [String]) { self.responses = responses }

    func run(_ command: RuntimeCommand) async throws -> CommandResult {
        commands.append(command)
        guard !responses.isEmpty else { throw SapphireError.invalidResponse("Unexpected command.") }
        let response = responses.removeFirst()
        if response == "wait-for-cancellation" {
            do { try await Task.sleep(for: .seconds(30)) }
            catch {
                cancellations += 1
                throw error
            }
        }
        return CommandResult(stdout: response)
    }
}

@MainActor
struct AppModelTests {
    private func waitUntil(_ condition: @MainActor () -> Bool) async throws {
        let deadline = ContinuousClock.now.advanced(by: .seconds(3))
        while !condition() && .now < deadline {
            try await Task.sleep(for: .milliseconds(10))
        }
        #expect(condition())
    }

    @Test func staleSnapshotCannotReplaceSavedVMLimitsWithEngineDefaults() async throws {
        let directory = URL(fileURLWithPath: "/tmp").appendingPathComponent("sapphire-model-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: directory) }
        let runner = ModelRunner([
            #"{"running":false}"#,
            #"{"running":true,"cpus":2,"memoryMiB":1024}"#,
        ])
        let model = SapphireModel(runtime: SapphireRuntime(
            paths: SapphirePaths(root: directory), runner: runner, enginePath: "/usr/bin/true"
        ))
        model.vmCPUs = 2
        model.vmMemoryMiB = 1024
        model.snapshot.virtualMachine.running = true
        model.perform("Start workload", success: "Done", ensureVM: true) {}
        try await waitUntil { !model.isBusy }
        let commands = await runner.commands
        #expect(commands.count == 2)
        #expect(commands.last?.arguments == [
            "start", "--json", "--cpus", "2", "--memory-mib", "1024",
        ])
        #expect(model.issue == nil)
    }

    @Test func closingWorkspacePreventsActionCompletionRefresh() async throws {
        let runner = ModelRunner([#"{"running":false}"#])
        let model = SapphireModel(runtime: SapphireRuntime(runner: runner, enginePath: "/usr/bin/true"))
        model.activate()
        try await waitUntil { model.hasLoaded && !model.isRefreshing }
        model.perform("Complete work", success: "Done") {
            try await Task.sleep(for: .milliseconds(50))
        }
        model.cancelRefresh()
        try await waitUntil { !model.isBusy }
        #expect(await runner.commands.count == 1)
        #expect(model.issue == nil)
    }

    @Test func actionCompletionRefreshCancelsWhenWorkspaceCloses() async throws {
        let runner = ModelRunner([#"{"running":false}"#, "wait-for-cancellation"])
        let model = SapphireModel(runtime: SapphireRuntime(runner: runner, enginePath: "/usr/bin/true"))
        model.activate()
        try await waitUntil { model.hasLoaded && !model.isRefreshing }
        model.perform("Complete work", success: "Done") {}
        try await waitUntil { model.isRefreshing }
        model.cancelRefresh()
        try await waitUntil { !model.isRefreshing }
        #expect(await runner.cancellations == 1)
        #expect(model.issue == nil)
    }

    @Test func RPCServiceStartupDoesNotRequireAHostDockerCLI() {
        let model = SapphireModel()
        model.dependencies = DependencyStatus(engine: "/usr/bin/true", docker: nil, guestAssets: true)
        #expect(model.canStartDocker)
    }
}

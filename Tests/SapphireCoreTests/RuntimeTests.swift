import Foundation
import Testing
@testable import SapphireCore

private actor ScriptedRunner: CommandRunning {
    private var responses: [CommandResult]
    private(set) var commands: [RuntimeCommand] = []

    init(_ responses: [String]) {
        self.responses = responses.map { CommandResult(stdout: $0) }
    }

    func run(_ command: RuntimeCommand) async throws -> CommandResult {
        commands.append(command)
        guard !responses.isEmpty else { throw SapphireError.invalidResponse("Unexpected command.") }
        return responses.removeFirst()
    }
}

struct RuntimeTests {
    @Test func engineEnvelopeAcceptsArrayResult() async throws {
        let runner = ScriptedRunner([
            #"{"running":true,"pid":42,"cpus":2,"memoryMiB":1024,"backend":"hypervisor"}"#,
            #"{"ok":true,"result":[{"name":"dev","status":"running","cpus":2}],"error":""}"#
        ])
        let runtime = SapphireRuntime(runner: runner, enginePath: "/usr/bin/true")
        let machines = try await runtime.machines()
        #expect(machines.count == 1)
        #expect(machines.first?.name == "dev")
    }

    @Test func engineEnvelopeAcceptsStringResult() async throws {
        let runner = ScriptedRunner([#"{"ok":true,"result":"apiVersion: v1\n","error":""}"#])
        let runtime = SapphireRuntime(runner: runner, enginePath: "/usr/bin/true")
        #expect(try await runtime.kubernetes("kubeconfig") == "apiVersion: v1\n")
    }

    @Test func machineExecPreservesFailureAndLiteralArguments() async throws {
        let runner = ScriptedRunner([#"{"ok":true,"result":{"stdout":"out","stderr":"err","exitCode":23}}"#])
        let runtime = SapphireRuntime(runner: runner, enginePath: "/usr/bin/true")
        let result = try await runtime.machineCommand("dev", arguments: ["printf", "$(touch /tmp/no)", "a b"])
        #expect(result.exitCode == 23)
        #expect(result.stdout == "out")
        let commands = await runner.commands
        let request = try #require(commands.first?.arguments.last)
        let json = try #require(JSONSerialization.jsonObject(with: Data(request.utf8)) as? [String: Any])
        #expect(json["arguments"] as? [String] == ["printf", "$(touch /tmp/no)", "a b"])
    }

    @Test func failedGuestEnvelopeIsReported() async throws {
        let runner = ScriptedRunner([#"{"ok":false,"result":null,"error":"memory limit unsupported"}"#])
        let runtime = SapphireRuntime(runner: runner, enginePath: "/usr/bin/true")
        do {
            try await runtime.stopDocker()
            Issue.record("A failed guest operation was accepted.")
        } catch {
            #expect(error.localizedDescription == "memory limit unsupported")
        }
    }

    @Test func dockerUsesOwnSocketAndClearsConflictingContext() async throws {
        let paths = SapphirePaths(root: URL(fileURLWithPath: "/tmp/sapphire-core-test"))
        let runner = ScriptedRunner([""])
        let runtime = SapphireRuntime(paths: paths, runner: runner)
        try await runtime.docker(["ps"])
        let command = try #require(await runner.commands.first)
        #expect(command.arguments == ["--host", "unix:///tmp/sapphire-core-test/docker.sock", "ps"])
        #expect(command.environment["DOCKER_CONTEXT"] == nil)
        #expect(command.environment["DOCKER_TLS_VERIFY"] == nil)
    }

    @Test func invalidMachineNamesNeverReachEngine() async throws {
        let runner = ScriptedRunner([])
        let runtime = SapphireRuntime(runner: runner, enginePath: "/usr/bin/true")
        await #expect(throws: SapphireError.self) {
            try await runtime.deleteMachine("../another-machine")
        }
        #expect(await runner.commands.isEmpty)
    }

    @Test func unsupportedMachineIntegrationIsRejected() {
        #expect(throws: SapphireError.self) {
            try MachineConfiguration(name: "dev", rosetta: true).validate()
        }
        #expect(throws: SapphireError.self) {
            try MachineConfiguration(name: "dev", shareHome: true).validate()
        }
        #expect(throws: SapphireError.self) {
            try MachineConfiguration(name: "docker", docker: true).validate()
        }
    }

    @Test func imageTagsHaveDistinctUIIdentityAndCorrectCLIReference() throws {
        let images = try JSONLines.decode(DockerImage.self, from: """
        {"id":"sha256:abc","repository":"example","tag":"one","size":"1 MB","created":"now"}
        {"id":"sha256:abc","repository":"example","tag":"two","size":"1 MB","created":"now"}
        """)
        #expect(images[0].id != images[1].id)
        #expect(images[0].imageID == images[1].imageID)
        #expect(images[0].reference == "example:one")
    }

    @Test func explicitVMLimitsCannotSilentlyBeIgnored() async throws {
        let root = URL(fileURLWithPath: "/tmp").appendingPathComponent("sapphire-test-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: root) }
        let runner = ScriptedRunner([#"{"running":true,"cpus":4,"memoryMiB":4096,"backend":"hypervisor"}"#])
        let runtime = SapphireRuntime(paths: SapphirePaths(root: root), runner: runner, enginePath: "/usr/bin/true")
        await #expect(throws: SapphireError.self) {
            try await runtime.startVM(cpus: 2)
        }
        #expect(await runner.commands.count == 1)
    }

    @Test func workspaceInventoryAvoidsDockerCLIProcesses() async throws {
        let runner = ScriptedRunner([
            #"{"running":true,"cpus":2,"memoryMiB":1024,"warnings":["port 8080 unavailable"]}"#,
            #"{"ok":true,"result":[]}"#,
            #"{"ok":true,"result":{"containers":[],"images":[],"volumes":[],"networks":[],"projects":[{"Name":"dev","Status":"running(1)","ConfigFiles":"/workspace/compose.yaml"}]}}"#
        ])
        let runtime = SapphireRuntime(runner: runner, enginePath: "/usr/bin/true")
        let snapshot = try await runtime.snapshot()
        #expect(snapshot.engineRunning)
        #expect(snapshot.projects.first?.name == "dev")
        #expect(snapshot.warnings == ["port 8080 unavailable"])
        #expect(await runner.commands.count == 3)
        #expect(await runner.commands.allSatisfy { $0.executable == "/usr/bin/true" })
    }

    @Test func cancelledProcessThatIgnoresTermIsKilled() async throws {
        let task = Task {
            try await ProcessRunner().run(RuntimeCommand(
                "/bin/sh", ["-c", "trap '' TERM; exec /usr/bin/perl -e '$SIG{TERM}=\"IGNORE\"; sleep 30'"],
                timeout: 30
            ))
        }
        try await Task.sleep(for: .milliseconds(250))
        let start = ContinuousClock.now
        task.cancel()
        do {
            _ = try await task.value
            Issue.record("Cancellation returned a command result.")
        } catch is CancellationError {
            #expect(start.duration(to: .now) < .seconds(8))
        }
    }

    @Test func containerTerminalUsesTheCLIShippedBesideItsEngine() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent("sapphire-cli-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        for name in ["sapphire", "sapphire-engine"] {
            let path = directory.appendingPathComponent(name)
            try Data("#!/bin/sh\nexit 0\n".utf8).write(to: path)
            try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: path.path)
        }
        let runtime = SapphireRuntime(
            runner: ScriptedRunner([]), enginePath: directory.appendingPathComponent("sapphire-engine").path
        )
        let command = try await runtime.terminalCommand(container: "example")
        #expect(command.contains(ShellQuote.quote(directory.appendingPathComponent("sapphire").path)))
    }
}

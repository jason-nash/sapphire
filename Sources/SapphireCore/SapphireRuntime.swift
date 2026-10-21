import Foundation

private struct EngineRequest: Encodable {
    var action: String
    var name: String?
    var destination: String?
    var configuration: MachineConfiguration?
    var arguments: [String]?
}

private struct EngineResponse<T: Decodable>: Decodable {
    let ok: Bool
    let result: T?
    let error: String?
}

private struct EngineEnvelope: Decodable {
    let ok: Bool
    let error: String?
}

private struct IgnoredResult: Decodable {
    init() {}
    init(from decoder: any Decoder) throws {}
}

public actor SapphireRuntime {
    public nonisolated let paths: SapphirePaths
    private let runner: any CommandRunning
    private let configuredEngine: String?

    public init(paths: SapphirePaths = SapphirePaths(), runner: any CommandRunning = ProcessRunner(),
                enginePath: String? = nil) {
        self.paths = paths
        self.runner = runner
        self.configuredEngine = enginePath
    }

    public func dependencies() -> DependencyStatus {
        DependencyStatus(
            engine: engineExecutable(),
            docker: ExecutableFinder.find("docker"),
            guestAssets: ["kernel", "initrd.gz", "rootfs.img"].allSatisfy {
                FileManager.default.fileExists(atPath: paths.assetsDirectory.appendingPathComponent($0).path)
            }
        )
    }

    public func status() async throws -> VMStatus {
        let result = try await engine(["status", "--json"])
        return try JSONDecoder().decode(VMStatus.self, from: Data(result.stdout.utf8))
    }

    public func machines() async throws -> [Machine] {
        guard try await status().running else { return [] }
        return try await request(EngineRequest(action: "machine.list"), as: [Machine].self)
    }

    public func createMachine(_ configuration: MachineConfiguration, start: Bool = true) async throws {
        try configuration.validate()
        try paths.prepare()
        try await startVM()
        let _: IgnoredResult = try await request(
            EngineRequest(action: "machine.create", configuration: configuration), timeout: 900
        )
        if start { try await startMachine(configuration.name) }
    }

    public func startVM(cpus: Int? = nil, memoryMiB: Int? = nil) async throws {
        var arguments = ["start", "--json"]
        if let cpus {
            guard (1...64).contains(cpus) else { throw SapphireError.invalidInput("Use 1-64 VM CPUs.") }
            arguments += ["--cpus", String(cpus)]
        }
        if let memoryMiB {
            guard (256...262144).contains(memoryMiB) else {
                throw SapphireError.invalidInput("Use 256-262144 MiB of VM memory.")
            }
            arguments += ["--memory-mib", String(memoryMiB)]
        }
        try paths.prepare()
        let result = try await engine(arguments, timeout: 180)
        let status = try JSONDecoder().decode(VMStatus.self, from: Data(result.stdout.utf8))
        guard status.running else { throw SapphireError.unavailable("The VM did not become ready.") }
        if let cpus, status.cpus != cpus {
            throw SapphireError.unavailable("The running VM has \(status.cpus) CPUs. Stop it before changing its CPU limit.")
        }
        if let memoryMiB, status.memoryMiB != memoryMiB {
            throw SapphireError.unavailable("The running VM has a \(status.memoryMiB) MiB limit. Stop it before changing its memory limit.")
        }
    }

    public func stopVM() async throws {
        try await engine(["stop"], timeout: 75)
    }

    public func startMachine(_ name: String) async throws {
        try ResourceName.validate(name)
        try await startVM()
        let _: IgnoredResult = try await request(EngineRequest(action: "machine.start", name: name), timeout: 180)
    }

    public func stopMachine(_ name: String) async throws {
        try ResourceName.validate(name)
        let _: IgnoredResult = try await request(EngineRequest(action: "machine.stop", name: name), timeout: 45)
    }

    public func deleteMachine(_ name: String) async throws {
        try ResourceName.validate(name)
        let _: IgnoredResult = try await request(EngineRequest(action: "machine.delete", name: name))
    }

    public func cloneMachine(_ source: String, to destination: String) async throws {
        try ResourceName.validate(source)
        try ResourceName.validate(destination)
        let _: IgnoredResult = try await request(
            EngineRequest(action: "machine.clone", name: source, destination: destination), timeout: 180
        )
    }

    public func startDocker() async throws {
        try await startVM()
        let _: IgnoredResult = try await request(EngineRequest(action: "docker.start"), timeout: 120)
    }

    public func stopDocker() async throws {
        let _: IgnoredResult = try await request(EngineRequest(action: "docker.stop"), timeout: 45)
    }

    public func kubernetes(_ operation: String) async throws -> String {
        guard ["start", "stop", "status", "kubeconfig"].contains(operation) else {
            throw SapphireError.invalidInput("Unknown Kubernetes operation.")
        }
        if operation == "start" { try await startDocker() }
        let result = try await engineCall(EngineRequest(action: "k8s.\(operation)"), timeout: 300)
        let json = try JSONSerialization.jsonObject(with: Data(result.utf8)) as? [String: Any]
        if let text = json?["result"] as? String { return text }
        return result
    }

    public func machineCommand(_ name: String, arguments: [String]) async throws -> CommandResult {
        try ResourceName.validate(name)
        guard !arguments.isEmpty else { throw SapphireError.invalidInput("A command is required.") }
        return try await request(
            EngineRequest(action: "machine.exec", name: name, arguments: arguments),
            as: GuestCommandResult.self, timeout: 3600
        ).commandResult
    }

    @discardableResult
    public func docker(_ arguments: [String], timeout: TimeInterval = 60,
                       directory: URL? = nil) async throws -> CommandResult {
        let result = try await runner.run(RuntimeCommand(
            "docker", ["--host", paths.dockerHost] + arguments,
            environment: paths.environment, directory: directory, timeout: timeout
        ))
        return try result.requireSuccess("docker")
    }

    public func logs(_ id: String, tail: Int = 300) async throws -> String {
        let result = try await docker(["container", "logs", "--tail", String(max(0, min(tail, 10_000))), "--", id])
        return result.stdout + result.stderr
    }

    public func inspect(_ kind: String, id: String) async throws -> String {
        guard ["container", "image", "volume", "network"].contains(kind) else {
            throw SapphireError.invalidInput("Unsupported Docker resource type.")
        }
        return try await docker([kind, "inspect", "--", id]).stdout
    }

    public func snapshot() async throws -> RuntimeSnapshot {
        var snapshot = RuntimeSnapshot()
        snapshot.virtualMachine = try await status()
        snapshot.warnings = snapshot.virtualMachine.warnings
        guard snapshot.virtualMachine.running else { return snapshot }
        do {
            snapshot.machines = try await request(EngineRequest(action: "machine.list"), as: [Machine].self)
        }
        catch { snapshot.warnings.append(error.localizedDescription) }
        do {
            let inventory = try await request(EngineRequest(action: "docker.inventory"), as: DockerInventory.self)
            snapshot.containers = inventory.containers
            snapshot.images = inventory.images
            snapshot.volumes = inventory.volumes
            snapshot.networks = inventory.networks
            snapshot.projects = inventory.projects
            snapshot.engineRunning = true
        } catch {
            snapshot.warnings.append(error.localizedDescription)
        }
        return snapshot
    }

    public func terminalCommand(machine: String? = nil, container: String? = nil) throws -> String {
        if let machine {
            try ResourceName.validate(machine)
            throw SapphireError.unavailable("Interactive machine terminals are not implemented. Run sapphire exec \(machine) -- COMMAND [ARG ...].")
        }
        guard let container else {
            throw SapphireError.invalidInput("Select a container to generate a terminal command.")
        }
        let executable = CommandLine.arguments.first ?? "sapphire"
        let sibling = URL(fileURLWithPath: executable).deletingLastPathComponent().appendingPathComponent("sapphire").path
        let engineSibling = engineExecutable().map {
            URL(fileURLWithPath: $0).deletingLastPathComponent().appendingPathComponent("sapphire").path
        }
        guard let cli = [sibling, engineSibling, ExecutableFinder.find("sapphire")]
            .compactMap({ $0 }).first(where: { FileManager.default.isExecutableFile(atPath: $0) }) else {
            throw SapphireError.missingDependency("sapphire")
        }
        let args = [cli, "docker", "exec", "-it", "--", container, "/bin/sh"]
        return "SAPPHIRE_HOME=\(ShellQuote.quote(paths.root.path)) " + args.map(ShellQuote.quote).joined(separator: " ")
    }

    private func request<T: Decodable>(_ request: EngineRequest, as: T.Type = T.self,
                                       timeout: TimeInterval = 60) async throws -> T {
        let text = try await engineCall(request, timeout: timeout)
        let response = try JSONDecoder().decode(EngineResponse<T>.self, from: Data(text.utf8))
        guard response.ok else { throw SapphireError.unavailable(response.error ?? "Guest operation failed.") }
        guard let value = response.result else {
            // Commands that return JSON null have no result payload.
            if let empty = IgnoredResult() as? T { return empty }
            throw SapphireError.invalidResponse("The guest returned no result for \(request.action).")
        }
        return value
    }

    private func engineCall(_ request: EngineRequest, timeout: TimeInterval) async throws -> String {
        let data = try JSONEncoder().encode(request)
        let result = try await engine(["call", String(decoding: data, as: UTF8.self)], timeout: timeout)
        let response = try JSONDecoder().decode(EngineEnvelope.self, from: Data(result.stdout.utf8))
        guard response.ok else { throw SapphireError.unavailable(response.error ?? "Guest operation failed.") }
        return result.stdout
    }

    @discardableResult
    private func engine(_ arguments: [String], timeout: TimeInterval = 30) async throws -> CommandResult {
        guard let executable = engineExecutable() else { throw SapphireError.missingDependency("sapphire-engine") }
        return try await runner.run(RuntimeCommand(
            executable, arguments, environment: paths.environment, timeout: timeout
        )).requireSuccess("sapphire-engine")
    }

    private func engineExecutable() -> String? {
        let env = ProcessInfo.processInfo.environment
        let sibling = URL(fileURLWithPath: CommandLine.arguments.first ?? "")
            .deletingLastPathComponent().appendingPathComponent("sapphire-engine").path
        return [configuredEngine, env["SAPPHIRE_ENGINE"], sibling,
                FileManager.default.currentDirectoryPath + "/engine/build/sapphire-engine",
                FileManager.default.currentDirectoryPath + "/engine/bin/sapphire-engine",
                ExecutableFinder.find("sapphire-engine")]
            .compactMap { $0 }.first { FileManager.default.isExecutableFile(atPath: $0) }
    }

    private struct GuestCommandResult: Decodable {
        let stdout: String
        let stderr: String
        let exitCode: Int32
        let truncated: Bool?
        var commandResult: CommandResult {
            let notice = truncated == true ? "\n[sapphire: guest command output was truncated]\n" : ""
            return CommandResult(stdout: stdout, stderr: stderr + notice, exitCode: exitCode)
        }
    }

    private struct DockerInventory: Decodable {
        let containers: [DockerContainer]
        let images: [DockerImage]
        let volumes: [DockerVolume]
        let networks: [DockerNetwork]
        let projects: [ComposeProject]
    }
}

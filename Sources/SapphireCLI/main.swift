import Darwin
import Foundation
import SapphireCore

@main
struct SapphireCLI {
    static func main() async {
        do {
            try await run(Array(CommandLine.arguments.dropFirst()))
        } catch {
            FileHandle.standardError.write(Data("sapphire: \(error.localizedDescription)\n".utf8))
            exit(1)
        }
    }

    private static func run(_ arguments: [String]) async throws {
        let runtime = SapphireRuntime()
        guard let command = arguments.first else { print(help); return }
        let rest = Array(arguments.dropFirst())
        switch command {
        case "help", "--help", "-h":
            print(help)
        case "--version", "version":
            print("Sapphire 0.1.0-dev (direct Hypervisor.framework)")
        case "doctor":
            let status = await runtime.dependencies()
            print("Host: \(ProcessInfo.processInfo.operatingSystemVersionString)")
            print("VM backend: Hypervisor.framework")
            print("Engine: \(status.engine ?? "missing")")
            print("Docker CLI: \(status.docker ?? "missing")")
            print("Guest assets: \(status.guestAssets ? "present" : "missing")")
            print("Sapphire home: \(runtime.paths.root.path)")
            if !status.ready { throw SapphireError.unavailable("Build and install the missing dependencies with scripts/build.sh.") }
        case "start":
            let options = try numericOptions(rest, allowed: ["--cpus", "--memory-mib"])
            try await runtime.startVM(cpus: options["--cpus"], memoryMiB: options["--memory-mib"])
            print("Sapphire VM started.")
        case "stop":
            guard rest.isEmpty else { throw usage("stop") }
            try await runtime.stopVM()
            print("Sapphire VM stopped.")
        case "status":
            let status = try await runtime.status()
            if rest == ["--json"] {
                try printJSON(status)
            } else {
                guard rest.isEmpty else { throw usage("status [--json]") }
                print(status.running ? "Running: \(status.cpus) CPUs, \(status.memoryMiB) MiB limit, PID \(status.pid)" : "Stopped")
            }
        case "list", "ls":
            guard rest.isEmpty || rest == ["--json"] else { throw usage("list [--json]") }
            let machines = try await runtime.machines()
            if rest == ["--json"] {
                try printJSON(machines)
            } else {
                print("NAME\tSTATUS\tCPUS\tMEMORY LIMIT")
                for machine in machines {
                    print("\(machine.name)\t\(machine.status)\t\(machine.cpus)\t\(machine.memory / (1 << 20)) MiB")
                }
            }
        case "create":
            let configuration = try parseCreate(rest)
            try await runtime.createMachine(configuration)
            print("Created \(configuration.name).")
        case "machine":
            try await machine(rest, runtime: runtime)
        case "exec", "shell":
            guard let name = rest.first else { throw usage("exec NAME -- COMMAND [ARG ...]") }
            var commandArguments = Array(rest.dropFirst())
            if commandArguments.first == "--" { commandArguments.removeFirst() }
            guard !commandArguments.isEmpty else {
                throw SapphireError.unavailable("Interactive machine terminals are not implemented yet. Use sapphire exec NAME -- COMMAND [ARG ...].")
            }
            let result = try await runtime.machineCommand(name, arguments: commandArguments)
            FileHandle.standardOutput.write(Data(result.stdout.utf8))
            FileHandle.standardError.write(Data(result.stderr.utf8))
            exit(result.exitCode)
        case "docker":
            if rest.first == "start", rest.count == 1 {
                try await runtime.startDocker()
            } else if rest.first == "stop", rest.count == 1 {
                try await runtime.stopDocker()
            } else {
                try await runtime.startDocker()
                try replaceProcess("docker", arguments: ["--host", runtime.paths.dockerHost] + rest,
                                   environment: runtime.paths.environment)
            }
        case "compose":
            try await runtime.startDocker()
            try replaceProcess("docker", arguments: ["--host", runtime.paths.dockerHost, "compose"] + rest,
                               environment: runtime.paths.environment)
        case "k8s":
            guard rest.count == 1, let operation = rest.first else {
                throw usage("k8s start|stop|status|kubeconfig")
            }
            print(try await runtime.kubernetes(operation))
        case "env":
            guard rest.isEmpty else { throw usage("env") }
            print("unset DOCKER_CONTEXT DOCKER_TLS_VERIFY DOCKER_CERT_PATH DOCKER_TLS DOCKER_TLS_CERTDIR")
            print("export DOCKER_HOST=\(ShellQuote.quote(runtime.paths.dockerHost))")
        default:
            throw SapphireError.invalidInput("Unknown command '\(command)'. Run sapphire --help.")
        }
    }

    private static func machine(_ arguments: [String], runtime: SapphireRuntime) async throws {
        guard let operation = arguments.first else { throw usage("machine start|stop|delete|clone NAME [DESTINATION]") }
        let rest = Array(arguments.dropFirst())
        guard let name = rest.first else { throw usage("machine \(operation) NAME") }
        switch operation {
        case "start", "stop":
            guard rest.count == 1 else { throw usage("machine \(operation) NAME") }
            if operation == "start" { try await runtime.startMachine(name) }
            else { try await runtime.stopMachine(name) }
        case "delete":
            guard rest.count == 2, rest[1] == "--yes" else {
                throw usage("machine delete NAME --yes (permanently removes this machine's files)")
            }
            try await runtime.deleteMachine(name)
        case "clone":
            guard rest.count == 2 else { throw usage("machine clone NAME DESTINATION") }
            try await runtime.cloneMachine(name, to: rest[1])
        default:
            throw SapphireError.invalidInput("Unknown machine operation '\(operation)'.")
        }
    }

    private static func parseCreate(_ arguments: [String]) throws -> MachineConfiguration {
        guard let name = arguments.first, !name.hasPrefix("-") else {
            throw usage("create NAME [--distribution alpine] [--cpus N] [--memory N] [--disk N]")
        }
        var configuration = MachineConfiguration(name: name, distribution: .alpine)
        var index = 1
        while index < arguments.count {
            let flag = arguments[index]
            guard index + 1 < arguments.count else { throw SapphireError.invalidInput("Missing value for \(flag).") }
            let value = arguments[index + 1]
            switch flag {
            case "--distribution":
                guard let distribution = LinuxDistribution(rawValue: value) else {
                    throw SapphireError.invalidInput("Unknown distribution '\(value)'.")
                }
                configuration.distribution = distribution
            case "--cpus", "--memory", "--disk":
                guard let number = Int(value) else { throw SapphireError.invalidInput("\(flag) requires an integer.") }
                if flag == "--cpus" { configuration.cpus = number }
                if flag == "--memory" { configuration.memoryGiB = number }
                if flag == "--disk" { configuration.diskGiB = number }
            default:
                throw SapphireError.invalidInput("Unknown option '\(flag)'.")
            }
            index += 2
        }
        try configuration.validate()
        return configuration
    }

    private static func printJSON<T: Encodable>(_ value: T) throws {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        print(String(decoding: try encoder.encode(value), as: UTF8.self))
    }

    private static func numericOptions(_ arguments: [String], allowed: Set<String>) throws -> [String: Int] {
        guard arguments.count.isMultiple(of: 2) else { throw SapphireError.invalidInput("Every option requires a value.") }
        var values: [String: Int] = [:]
        for index in stride(from: 0, to: arguments.count, by: 2) {
            let flag = arguments[index]
            guard allowed.contains(flag), values[flag] == nil, let value = Int(arguments[index + 1]) else {
                throw SapphireError.invalidInput("Invalid, duplicate, or unknown option '\(flag)'.")
            }
            values[flag] = value
        }
        return values
    }

    private static func usage(_ syntax: String) -> SapphireError {
        .invalidInput("Usage: sapphire \(syntax)")
    }

    private static func replaceProcess(_ executable: String, arguments: [String],
                                       environment: [String: String]) throws {
        guard let path = ExecutableFinder.find(executable) else { throw SapphireError.missingDependency(executable) }
        for key in ["DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_TLS", "DOCKER_TLS_CERTDIR"] {
            unsetenv(key)
        }
        for (key, value) in environment { setenv(key, value, 1) }
        let values = ([path] + arguments).map { strdup($0) }
        defer { values.forEach { free($0) } }
        var pointers = values + [nil]
        _ = pointers.withUnsafeMutableBufferPointer { buffer in execv(path, buffer.baseAddress!) }
        throw SapphireError.unavailable("Could not execute \(executable): \(String(cString: strerror(errno))).")
    }

    private static let help = """
    Sapphire - a shared-kernel Linux runtime for Apple Silicon

    Usage: sapphire COMMAND
      doctor                         Check build, tools, and guest assets
      start [--cpus N --memory-mib N] Start Sapphire's own Hypervisor VM
      stop | status [--json]         Stop or inspect the VM
      list [--json]                  List Linux machines
      create NAME [OPTIONS]          Create a namespace-based Linux machine
      machine start|stop NAME        Start or stop a machine
      machine delete NAME --yes      Permanently delete a machine
      machine clone NAME DESTINATION Clone a stopped machine
      exec NAME -- COMMAND [ARGS]    Execute a command in a machine
      docker [ARGS]                  Start Docker on demand and run its CLI
      compose [ARGS]                 Run Docker Compose against Sapphire
      k8s start|stop|status|kubeconfig Manage Kubernetes in the shared VM
      env                            Print Sapphire Docker environment

    Create options: --distribution alpine --cpus N --memory GiB --disk GiB
    SAPPHIRE_HOME selects a separate runtime directory (default ~/.sapphire).
    Existing Docker contexts are preserved. Interactive machine PTYs, Rosetta,
    and host directory sharing are not implemented.
    """
}

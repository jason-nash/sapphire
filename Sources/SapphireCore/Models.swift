import Foundation

public enum SapphireError: Error, LocalizedError, Sendable {
    case invalidInput(String)
    case missingDependency(String)
    case commandFailed(String, Int32, String)
    case timedOut(String)
    case invalidResponse(String)
    case unavailable(String)

    public var errorDescription: String? {
        switch self {
        case .invalidInput(let message), .invalidResponse(let message),
             .unavailable(let message):
            return message
        case .missingDependency(let name):
            return "\(name) was not found. Build Sapphire with scripts/build.sh, or install the missing tool."
        case .commandFailed(let name, let code, let message):
            return "\(name) exited with status \(code): \(message.trimmingCharacters(in: .whitespacesAndNewlines))"
        case .timedOut(let name):
            return "\(name) timed out. Check the machine's logs before retrying."
        }
    }
}

public enum LinuxDistribution: String, CaseIterable, Codable, Identifiable, Sendable {
    case ubuntu, debian, fedora, alpine
    public var id: String { rawValue }
    public var title: String { rawValue.capitalized }
}

public struct MachineConfiguration: Codable, Sendable {
    public var name: String
    public var distribution: LinuxDistribution
    public var cpus: Int
    public var memoryGiB: Int
    public var diskGiB: Int
    public var docker: Bool
    public var rosetta: Bool
    public var shareHome: Bool

    public init(
        name: String, distribution: LinuxDistribution = .alpine,
        cpus: Int = 2, memoryGiB: Int = 2, diskGiB: Int = 16,
        docker: Bool = false, rosetta: Bool = false, shareHome: Bool = false
    ) {
        self.name = name
        self.distribution = distribution
        self.cpus = cpus
        self.memoryGiB = memoryGiB
        self.diskGiB = diskGiB
        self.docker = docker
        self.rosetta = rosetta
        self.shareHome = shareHome
    }

    public func validate() throws {
        try ResourceName.validate(name)
        guard (1...64).contains(cpus), (1...256).contains(memoryGiB),
              (8...2048).contains(diskGiB) else {
            throw SapphireError.invalidInput("Use 1-64 CPUs, 1-256 GiB memory, and 8-2048 GiB disk.")
        }
        guard !docker else {
            throw SapphireError.invalidInput("Use startDocker for the shared guest engine.")
        }
        guard name != "docker" else {
            throw SapphireError.invalidInput("'docker' is reserved for Sapphire's container engine.")
        }
        guard !rosetta, !shareHome else {
            throw SapphireError.invalidInput("Rosetta and host directory sharing are not implemented.")
        }
    }
}

public enum ResourceName {
    public static func validate(_ name: String) throws {
        guard !name.isEmpty, name.utf8.count <= 63,
              name.range(of: #"^[a-z0-9][a-z0-9_-]*$"#, options: .regularExpression) != nil else {
            throw SapphireError.invalidInput("Use a name of 1-63 lowercase letters, digits, hyphens, or underscores, starting with a letter or digit.")
        }
    }
}

public struct Machine: Codable, Identifiable, Sendable {
    public var name: String
    public var status: String
    public var arch: String
    public var cpus: Int
    public var memory: UInt64
    public var disk: UInt64
    public var dir: String
    public var sshLocalPort: Int
    public var id: String { name }
    public var isRunning: Bool { status.lowercased() == "running" }
    public var isDocker: Bool { name == "docker" }

    public init(name: String, status: String, arch: String = "aarch64", cpus: Int = 0,
                memory: UInt64 = 0, disk: UInt64 = 0, dir: String = "", sshLocalPort: Int = 0) {
        self.name = name
        self.status = status
        self.arch = arch
        self.cpus = cpus
        self.memory = memory
        self.disk = disk
        self.dir = dir
        self.sshLocalPort = sshLocalPort
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decode(String.self, forKey: .name)
        status = try c.decode(String.self, forKey: .status)
        arch = try c.decodeIfPresent(String.self, forKey: .arch) ?? ""
        cpus = try c.decodeIfPresent(Int.self, forKey: .cpus) ?? 0
        memory = try c.decodeIfPresent(UInt64.self, forKey: .memory) ?? 0
        disk = try c.decodeIfPresent(UInt64.self, forKey: .disk) ?? 0
        dir = try c.decodeIfPresent(String.self, forKey: .dir) ?? ""
        sshLocalPort = try c.decodeIfPresent(Int.self, forKey: .sshLocalPort) ?? 0
    }
}

public struct DockerContainer: Codable, Identifiable, Sendable {
    public let id: String
    public let name: String
    public let image: String
    public let state: String
    public let status: String
    public let ports: String
    public let project: String
    public var isRunning: Bool { state.lowercased() == "running" }

    public init(id: String, name: String, image: String, state: String,
                status: String, ports: String = "", project: String = "") {
        self.id = id
        self.name = name
        self.image = image
        self.state = state
        self.status = status
        self.ports = ports
        self.project = project
    }
}

public struct DockerImage: Codable, Identifiable, Sendable {
    public let imageID: String
    public let repository: String
    public let tag: String
    public let size: String
    public let created: String
    public var id: String { "\(imageID)|\(repository)|\(tag)" }
    public var reference: String { repository == "<none>" ? imageID : "\(repository):\(tag)" }
    enum CodingKeys: String, CodingKey {
        case imageID = "id", repository, tag, size, created
    }

    public init(id: String, repository: String, tag: String, size: String, created: String) {
        self.imageID = id
        self.repository = repository
        self.tag = tag
        self.size = size
        self.created = created
    }
}

public struct DockerVolume: Codable, Identifiable, Sendable {
    public let name: String
    public let driver: String
    public var id: String { name }
    public init(name: String, driver: String) {
        self.name = name
        self.driver = driver
    }
}

public struct DockerNetwork: Codable, Identifiable, Sendable {
    public let id: String
    public let name: String
    public let driver: String
    public let scope: String
    public init(id: String, name: String, driver: String, scope: String) {
        self.id = id
        self.name = name
        self.driver = driver
        self.scope = scope
    }
}

public struct ComposeProject: Codable, Identifiable, Sendable {
    public let name: String
    public let status: String
    public let configFiles: String
    public var id: String { name }
    enum CodingKeys: String, CodingKey {
        case name = "Name", status = "Status", configFiles = "ConfigFiles"
    }
    public init(name: String, status: String, configFiles: String) {
        self.name = name
        self.status = status
        self.configFiles = configFiles
    }
}

public struct RuntimeSnapshot: Codable, Sendable {
    public var virtualMachine = VMStatus()
    public var machines: [Machine] = []
    public var containers: [DockerContainer] = []
    public var images: [DockerImage] = []
    public var volumes: [DockerVolume] = []
    public var networks: [DockerNetwork] = []
    public var projects: [ComposeProject] = []
    public var warnings: [String] = []
    public var engineRunning = false
    public init() {}
}

public struct DependencyStatus: Codable, Sendable {
    public let engine: String?
    public let docker: String?
    public let guestAssets: Bool
    public var ready: Bool { engine != nil && docker != nil && guestAssets }
    public init(engine: String?, docker: String?, guestAssets: Bool) {
        self.engine = engine
        self.docker = docker
        self.guestAssets = guestAssets
    }
}

public struct VMStatus: Codable, Sendable {
    public var running: Bool
    public var pid: Int
    public var cpus: Int
    public var memoryMiB: Int
    public var backend: String
    public var warnings: [String]

    public init(running: Bool = false, pid: Int = 0, cpus: Int = 0,
                memoryMiB: Int = 0, backend: String = "hypervisor", warnings: [String] = []) {
        self.running = running
        self.pid = pid
        self.cpus = cpus
        self.memoryMiB = memoryMiB
        self.backend = backend
        self.warnings = warnings
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        running = try c.decodeIfPresent(Bool.self, forKey: .running) ?? false
        pid = try c.decodeIfPresent(Int.self, forKey: .pid) ?? 0
        cpus = try c.decodeIfPresent(Int.self, forKey: .cpus) ?? 0
        memoryMiB = try c.decodeIfPresent(Int.self, forKey: .memoryMiB) ?? 0
        backend = try c.decodeIfPresent(String.self, forKey: .backend) ?? "hypervisor"
        warnings = try c.decodeIfPresent([String].self, forKey: .warnings) ?? []
    }
}

public enum JSONLines {
    public static func decode<T: Decodable>(_ type: T.Type, from text: String) throws -> [T] {
        let decoder = JSONDecoder()
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return [] }
        if trimmed.hasPrefix("[") {
            return try decoder.decode([T].self, from: Data(trimmed.utf8))
        }
        return try trimmed.split(whereSeparator: \.isNewline).map {
            try decoder.decode(T.self, from: Data($0.utf8))
        }
    }
}

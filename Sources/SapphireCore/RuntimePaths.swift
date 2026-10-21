import Foundation

public struct SapphirePaths: Sendable {
    public let root: URL
    public let home: URL
    public var assetsDirectory: URL { root.appendingPathComponent("assets", isDirectory: true) }
    public var sharedDirectory: URL { root.appendingPathComponent("shared", isDirectory: true) }
    public var configurationDirectory: URL { root.appendingPathComponent("configurations", isDirectory: true) }
    public var dockerSocket: URL { root.appendingPathComponent("docker.sock") }
    public var controlSocket: URL { root.appendingPathComponent("control.sock") }
    public var dockerHost: String { "unix://\(dockerSocket.path)" }

    public init(root: URL? = nil, home: URL = FileManager.default.homeDirectoryForCurrentUser) {
        self.home = home.standardizedFileURL
        if let root {
            self.root = root.standardizedFileURL
        } else if let custom = ProcessInfo.processInfo.environment["SAPPHIRE_HOME"], !custom.isEmpty {
            self.root = URL(fileURLWithPath: NSString(string: custom).expandingTildeInPath).standardizedFileURL
        } else {
            self.root = home.appendingPathComponent(".sapphire", isDirectory: true)
        }
    }

    public func prepare() throws {
        // macOS limits sockaddr_un to 104 bytes, including the trailing NUL.
        guard dockerSocket.path.utf8.count < 100 else {
            throw SapphireError.invalidInput("SAPPHIRE_HOME is too long for macOS Unix sockets. Use a shorter absolute path.")
        }
        for directory in [root, sharedDirectory, configurationDirectory] {
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        }
    }

    public var environment: [String: String] {
        var env = ProcessInfo.processInfo.environment
        env["SAPPHIRE_HOME"] = root.path
        env["DOCKER_HOST"] = dockerHost
        for key in ["DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_TLS",
                    "DOCKER_TLS_CERTDIR"] {
            env.removeValue(forKey: key)
        }
        let inherited = env["PATH"] ?? ""
        env["PATH"] = inherited + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
        return env
    }
}

public enum ShellQuote {
    public static func quote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }
}

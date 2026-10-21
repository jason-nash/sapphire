import Foundation

public struct RuntimeCommand: Sendable {
    public let executable: String
    public let arguments: [String]
    public let environment: [String: String]
    public let directory: URL?
    public let timeout: TimeInterval

    public init(_ executable: String, _ arguments: [String],
                environment: [String: String] = [:], directory: URL? = nil,
                timeout: TimeInterval = 30) {
        self.executable = executable
        self.arguments = arguments
        self.environment = environment
        self.directory = directory
        self.timeout = timeout
    }
}

public struct CommandResult: Sendable {
    public let stdout: String
    public let stderr: String
    public let exitCode: Int32
    public init(stdout: String = "", stderr: String = "", exitCode: Int32 = 0) {
        self.stdout = stdout
        self.stderr = stderr
        self.exitCode = exitCode
    }

    @discardableResult
    public func requireSuccess(_ executable: String) throws -> Self {
        guard exitCode == 0 else {
            throw SapphireError.commandFailed(executable, exitCode, String((stderr.isEmpty ? stdout : stderr).suffix(4000)))
        }
        return self
    }
}

public protocol CommandRunning: Sendable {
    func run(_ command: RuntimeCommand) async throws -> CommandResult
}

public enum ExecutableFinder {
    public static func find(_ name: String) -> String? {
        let search = (ProcessInfo.processInfo.environment["PATH"] ?? "")
            + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
        return search.split(separator: ":").map { String($0) + "/" + name }
            .first { FileManager.default.isExecutableFile(atPath: $0) }
    }
}

public struct ProcessRunner: CommandRunning {
    public init() {}
    public func run(_ command: RuntimeCommand) async throws -> CommandResult {
        let running = RunningProcess(command)
        return try await withTaskCancellationHandler {
            try Task.checkCancellation()
            return try await withCheckedThrowingContinuation { continuation in
                running.start(continuation)
            }
        } onCancel: {
            running.cancel()
        }
    }
}

// Process lifecycle and continuation completion are synchronized by the lock.
private final class RunningProcess: @unchecked Sendable {
    private let command: RuntimeCommand
    private let process = Process()
    private let lock = NSLock()
    private var cancelled = false
    private var timeoutReached = false
    private var finished = false
    private var directory: URL?
    private var stdoutHandle: FileHandle?
    private var stderrHandle: FileHandle?

    init(_ command: RuntimeCommand) { self.command = command }

    func start(_ continuation: CheckedContinuation<CommandResult, any Error>) {
        do {
            let temp = FileManager.default.temporaryDirectory.appendingPathComponent("sapphire-\(UUID().uuidString)")
            try FileManager.default.createDirectory(at: temp, withIntermediateDirectories: true)
            directory = temp
            let out = temp.appendingPathComponent("stdout")
            let err = temp.appendingPathComponent("stderr")
            FileManager.default.createFile(atPath: out.path, contents: nil)
            FileManager.default.createFile(atPath: err.path, contents: nil)
            stdoutHandle = try FileHandle(forWritingTo: out)
            stderrHandle = try FileHandle(forWritingTo: err)
            guard let executable = command.executable.hasPrefix("/")
                    ? command.executable : ExecutableFinder.find(command.executable) else {
                throw SapphireError.missingDependency(command.executable)
            }
            process.executableURL = URL(fileURLWithPath: executable)
            process.arguments = command.arguments
            process.environment = command.environment.isEmpty ? ProcessInfo.processInfo.environment : command.environment
            process.currentDirectoryURL = command.directory
            process.standardInput = FileHandle.nullDevice
            process.standardOutput = stdoutHandle
            process.standardError = stderrHandle
            process.terminationHandler = { [self] process in
                lock.lock()
                finished = true
                let wasCancelled = cancelled
                let wasTimeout = timeoutReached
                lock.unlock()
                let stdout = (try? String(contentsOf: out, encoding: .utf8)) ?? ""
                let stderr = (try? String(contentsOf: err, encoding: .utf8)) ?? ""
                cleanup()
                if wasCancelled {
                    continuation.resume(throwing: CancellationError())
                } else if wasTimeout {
                    continuation.resume(throwing: SapphireError.timedOut(command.executable))
                } else {
                    continuation.resume(returning: CommandResult(stdout: stdout, stderr: stderr, exitCode: process.terminationStatus))
                }
            }
            lock.lock()
            if cancelled {
                lock.unlock()
                throw CancellationError()
            }
            do { try process.run() } catch {
                lock.unlock()
                throw error
            }
            lock.unlock()
            DispatchQueue.global().asyncAfter(deadline: .now() + command.timeout) { [weak self] in
                self?.expire()
            }
        } catch {
            cleanup()
            continuation.resume(throwing: error)
        }
    }

    func cancel() {
        lock.lock()
        cancelled = true
        let needsTermination = !finished && process.isRunning
        if needsTermination { process.terminate() }
        lock.unlock()
        if needsTermination { scheduleKill() }
    }

    private func expire() {
        lock.lock()
        guard !finished, process.isRunning else { lock.unlock(); return }
        timeoutReached = true
        process.terminate()
        lock.unlock()
        scheduleKill()
    }

    private func scheduleKill() {
        DispatchQueue.global().asyncAfter(deadline: .now() + 3) { [weak self] in
            guard let self else { return }
            self.lock.lock()
            if !self.finished, self.process.isRunning { kill(self.process.processIdentifier, SIGKILL) }
            self.lock.unlock()
        }
    }

    private func cleanup() {
        process.terminationHandler = nil
        try? stdoutHandle?.close()
        try? stderrHandle?.close()
        stdoutHandle = nil
        stderrHandle = nil
        if let directory { try? FileManager.default.removeItem(at: directory) }
        directory = nil
    }
}

import AppKit
import SapphireCore
import SwiftUI

enum WorkspaceSection: String, CaseIterable, Identifiable {
    case containers, compose, images, volumes, networks, machines, kubernetes

    var id: String { rawValue }

    var title: String {
        switch self {
        case .containers: "Containers"
        case .compose: "Compose"
        case .images: "Images"
        case .volumes: "Volumes"
        case .networks: "Networks"
        case .machines: "Linux Machines"
        case .kubernetes: "Kubernetes"
        }
    }

    var symbol: String {
        switch self {
        case .containers: "shippingbox"
        case .compose: "square.stack.3d.up"
        case .images: "square.3.layers.3d"
        case .volumes: "externaldrive"
        case .networks: "point.3.connected.trianglepath.dotted"
        case .machines: "desktopcomputer"
        case .kubernetes: "helm"
        }
    }

    var subtitle: String {
        switch self {
        case .containers: "A place for everything you're building."
        case .compose: "Your services, working together."
        case .images: "The starting point for your next container."
        case .volumes: "Persistent storage for your services."
        case .networks: "Connections inside your Linux environment."
        case .machines: "Independent Linux userspaces. One shared kernel."
        case .kubernetes: "A local cluster in the shared Linux environment."
        }
    }

    var addTitle: String {
        switch self {
        case .containers: "Run Container"
        case .compose: "Open Compose File"
        case .images: "Pull Image"
        case .volumes: "Create Volume"
        case .networks: "Create Network"
        case .machines: "Create Machine"
        case .kubernetes: "Start Kubernetes"
        }
    }
}

struct ResourceField: Identifiable {
    let label: String
    let value: String
    var id: String { label }
}

enum ResourceRecord: Identifiable {
    case container(DockerContainer)
    case project(ComposeProject)
    case image(DockerImage)
    case volume(DockerVolume)
    case network(DockerNetwork)
    case machine(Machine)

    var id: String {
        switch self {
        case .container(let item): "container:\(item.id)"
        case .project(let item): "compose:\(item.id)"
        // Multiple tags may refer to the same image ID.
        case .image(let item): "image:\(item.id)"
        case .volume(let item): "volume:\(item.id)"
        case .network(let item): "network:\(item.id)"
        case .machine(let item): "machine:\(item.id)"
        }
    }

    var name: String {
        switch self {
        case .container(let item): item.name
        case .project(let item): item.name
        case .image(let item): item.reference
        case .volume(let item): item.name
        case .network(let item): item.name
        case .machine(let item): item.name
        }
    }

    var subtitle: String {
        switch self {
        case .container(let item): item.image
        case .project(let item): item.configFiles.isEmpty ? "Compose project" : item.configFiles
        case .image(let item): "\(item.size)  /  \(item.created)"
        case .volume(let item): "\(item.driver) driver"
        case .network(let item): "\(item.driver)  /  \(item.scope)"
        case .machine(let item): "\(item.arch)  /  \(item.cpus) CPUs  /  \(formatBytes(item.memory))"
        }
    }

    var symbol: String {
        switch self {
        case .container: "shippingbox"
        case .project: "square.stack.3d.up"
        case .image: "square.3.layers.3d"
        case .volume: "externaldrive"
        case .network: "point.3.connected.trianglepath.dotted"
        case .machine: "desktopcomputer"
        }
    }

    var status: String {
        switch self {
        case .container(let item): item.state.capitalized
        case .project(let item): item.status
        case .machine(let item): item.status.capitalized
        case .image: "Local"
        case .volume: "Volume"
        case .network: "Network"
        }
    }

    var isRunning: Bool {
        switch self {
        case .container(let item): item.isRunning
        case .machine(let item): item.isRunning
        case .project(let item): item.status.lowercased().hasPrefix("running")
        default: false
        }
    }

    var hasLifecycle: Bool {
        switch self {
        case .container, .project, .machine: true
        default: false
        }
    }

    var supportsLogs: Bool {
        switch self {
        case .container, .project: true
        default: false
        }
    }

    var supportsShell: Bool {
        switch self {
        case .container: true
        default: false
        }
    }

    var canDelete: Bool {
        switch self {
        case .container, .machine: !isRunning
        case .network(let item): !["bridge", "host", "none"].contains(item.name)
        default: true
        }
    }

    var fields: [ResourceField] {
        switch self {
        case .container(let item):
            [
                .init(label: "Image", value: item.image),
                .init(label: "Container ID", value: item.id),
                .init(label: "Status", value: item.status),
                .init(label: "Ports", value: item.ports.isEmpty ? "No published ports" : item.ports),
                .init(label: "Compose project", value: item.project.isEmpty ? "None" : item.project),
            ]
        case .project(let item):
            [
                .init(label: "Project", value: item.name),
                .init(label: "Status", value: item.status),
                .init(label: "Configuration files", value: item.configFiles.isEmpty ? "Not reported" : item.configFiles),
            ]
        case .image(let item):
            [
                .init(label: "Repository", value: item.repository),
                .init(label: "Tag", value: item.tag),
                .init(label: "Image ID", value: item.imageID),
                .init(label: "Size", value: item.size),
                .init(label: "Created", value: item.created),
            ]
        case .volume(let item):
            [.init(label: "Name", value: item.name), .init(label: "Driver", value: item.driver)]
        case .network(let item):
            [
                .init(label: "Network ID", value: item.id),
                .init(label: "Driver", value: item.driver),
                .init(label: "Scope", value: item.scope),
            ]
        case .machine(let item):
            [
                .init(label: "Architecture", value: item.arch),
                .init(label: "CPU limit", value: String(item.cpus)),
                .init(label: "Memory limit", value: formatBytes(item.memory)),
                .init(label: "Guest directory", value: item.dir.isEmpty ? "Not reported" : item.dir),
                .init(label: "Kernel", value: "Shared Linux kernel"),
                .init(label: "Root access", value: "Shared guest administrator"),
            ]
        }
    }

    func matches(_ query: String) -> Bool {
        query.isEmpty || ([name, subtitle, status] + fields.map(\.value))
            .contains { $0.localizedCaseInsensitiveContains(query) }
    }
}

enum ResourceForm: Identifiable {
    case startVM
    case pullImage
    case runContainer(image: String = "")
    case createVolume
    case createNetwork
    case compose
    case createMachine
    case cloneMachine(Machine)

    var id: String {
        switch self {
        case .startVM: "vm"
        case .pullImage: "pull"
        case .runContainer(let image): "run:\(image)"
        case .createVolume: "volume"
        case .createNetwork: "network"
        case .compose: "compose"
        case .createMachine: "machine"
        case .cloneMachine(let machine): "clone:\(machine.name)"
        }
    }

    var title: String {
        switch self {
        case .startVM: "Start the shared Linux VM"
        case .pullImage: "Pull an image"
        case .runContainer: "Run a container"
        case .createVolume: "Create a volume"
        case .createNetwork: "Create a network"
        case .compose: "Bring up a Compose project"
        case .createMachine: "Create a Linux machine"
        case .cloneMachine: "Clone a Linux machine"
        }
    }

    var actionTitle: String {
        switch self {
        case .startVM: "Start VM"
        case .pullImage: "Pull Image"
        case .runContainer: "Run Container"
        case .createVolume: "Create Volume"
        case .createNetwork: "Create Network"
        case .compose: "Bring Up"
        case .createMachine: "Create & Start"
        case .cloneMachine: "Clone Machine"
        }
    }

    var needsDocker: Bool {
        switch self {
        case .startVM, .createMachine, .cloneMachine: false
        default: true
        }
    }
}

enum ConfirmedAction {
    case remove(ResourceRecord)
    case stopVM
    case stopDocker
    case stopKubernetes
}

struct ActionConfirmation: Identifiable {
    let id = UUID()
    let title: String
    let message: String
    let button: String
    let action: ConfirmedAction
}

struct AppIssue: Identifiable {
    let id = UUID()
    let title: String
    let message: String
}

@MainActor
final class SapphireModel: ObservableObject {
    let runtime: SapphireRuntime
    let isPreview: Bool
    @Published var snapshot = RuntimeSnapshot()
    @Published var dependencies: DependencyStatus?
    @Published var section: WorkspaceSection = .containers
    @Published var search = ""
    @Published var selectedID: String?
    @Published var presentedForm: ResourceForm?
    @Published var confirmation: ActionConfirmation?
    @Published var issue: AppIssue?
    @Published var successMessage: String?
    @Published var isRefreshing = false
    @Published var hasLoaded = false
    @Published var lastUpdated: Date?
    @Published var operationTitle: String?
    @Published var detailRevision = 0
    @Published var kubernetesStatus: String?
    @Published var kubernetesError: String?
    @Published var isLoadingKubernetes = false
    @Published var showAbout = false
    @Published var vmCPUs: Int
    @Published var vmMemoryMiB: Int
    private var refreshTask: Task<Void, Never>?
    private var refreshGeneration = UUID()
    private var operationTask: Task<Void, Never>?
    private var workspaceActive = false

    init(runtime: SapphireRuntime = SapphireRuntime(), isPreview: Bool = false) {
        self.runtime = runtime
        self.isPreview = isPreview
        let savedCPUs = UserDefaults.standard.integer(forKey: "sharedVMCPUs")
        let savedMemory = UserDefaults.standard.integer(forKey: "sharedVMMemoryMiB")
        vmCPUs = (1...64).contains(savedCPUs) ? savedCPUs : 2
        vmMemoryMiB = (256...262144).contains(savedMemory) ? savedMemory : 1024
    }

    var isBusy: Bool { operationTitle != nil }
    var canAct: Bool { !isBusy && !isRefreshing && !isPreview }
    var canStartVM: Bool { dependencies?.engine != nil && dependencies?.guestAssets == true }
    var canStartDocker: Bool { canStartVM }
    var selectedResource: ResourceRecord? { resources.first { $0.id == selectedID } }

    var resources: [ResourceRecord] {
        records(in: section)
            .filter { $0.matches(search.trimmingCharacters(in: .whitespacesAndNewlines)) }
            .sorted {
                if $0.isRunning != $1.isRunning { return $0.isRunning }
                return $0.name.localizedStandardCompare($1.name) == .orderedAscending
            }
    }

    func records(in section: WorkspaceSection) -> [ResourceRecord] {
        switch section {
        case .containers: snapshot.containers.map(ResourceRecord.container)
        case .compose: snapshot.projects.map(ResourceRecord.project)
        case .images: snapshot.images.map(ResourceRecord.image)
        case .volumes: snapshot.volumes.map(ResourceRecord.volume)
        case .networks: snapshot.networks.map(ResourceRecord.network)
        case .machines: snapshot.machines.map(ResourceRecord.machine)
        case .kubernetes: []
        }
    }

    func navigate(to section: WorkspaceSection) {
        self.section = section
        search = ""
        selectedID = resources.first?.id
    }

    func activate() {
        workspaceActive = true
        guard !isPreview, !isBusy else { return }
        if let refreshTask, !refreshTask.isCancelled { return }
        let previous = refreshTask
        let generation = UUID()
        refreshGeneration = generation
        refreshTask = Task { [weak self] in
            await previous?.value
            guard let self, !Task.isCancelled else { return }
            await refresh()
            if refreshGeneration == generation { refreshTask = nil }
        }
    }

    func cancelRefresh() {
        workspaceActive = false
        refreshTask?.cancel()
    }

    // Refreshes only follow explicit events. There is no timer or background loop.
    func refresh() async {
        guard !isPreview, !isRefreshing, !isBusy else { return }
        isRefreshing = true
        defer { isRefreshing = false }
        do {
            dependencies = await runtime.dependencies()
            try Task.checkCancellation()
            let next = try await runtime.snapshot()
            try Task.checkCancellation()
            snapshot = next
            if !next.virtualMachine.running {
                kubeconfig = nil
                kubernetesStatus = nil
            }
            lastUpdated = Date()
            hasLoaded = true
            reconcileSelection()
            detailRevision += 1
            if section == .kubernetes { await refreshKubernetes() }
        } catch is CancellationError {
            // An inactive or closed window cancels its read without presenting an error.
        } catch {
            hasLoaded = true
            issue = AppIssue(title: "Couldn't refresh Sapphire", message: error.localizedDescription)
        }
    }

    func refreshKubernetes() async {
        guard !isPreview, !isLoadingKubernetes, !isBusy else { return }
        guard snapshot.virtualMachine.running else {
            kubernetesStatus = nil
            kubernetesError = nil
            return
        }
        isLoadingKubernetes = true
        defer { isLoadingKubernetes = false }
        do {
            let text = try await runtime.kubernetes("status")
            try Task.checkCancellation()
            kubernetesStatus = prettyJSON(text)
            kubernetesError = nil
        } catch is CancellationError {
        } catch {
            kubernetesError = error.localizedDescription
        }
    }

    func reconcileSelection() {
        if !resources.contains(where: { $0.id == selectedID }) {
            selectedID = resources.first?.id
        }
    }

    func presentCreationForm() {
        guard canAct else { return }
        switch section {
        case .containers: presentedForm = .runContainer()
        case .compose: presentedForm = .compose
        case .images: presentedForm = .pullImage
        case .volumes: presentedForm = .createVolume
        case .networks: presentedForm = .createNetwork
        case .machines: presentedForm = .createMachine
        case .kubernetes: startKubernetes()
        }
    }

    func perform(
        _ title: String, success: String, ensureVM: Bool = false,
        action: @escaping @MainActor () async throws -> Void
    ) {
        guard canAct else { return }
        operationTitle = title
        successMessage = nil
        issue = nil
        operationTask = Task { [weak self] in
            guard let self else { return }
            do {
                if ensureVM {
                    let current = try await runtime.status()
                    if !current.running {
                        try await runtime.startVM(cpus: vmCPUs, memoryMiB: vmMemoryMiB)
                    }
                }
                try await action()
                successMessage = success
            } catch is CancellationError {
                issue = AppIssue(title: "\(title) was cancelled", message: "Refresh to check the current runtime state before retrying.")
            } catch {
                issue = AppIssue(title: title, message: error.localizedDescription)
            }
            operationTitle = nil
            operationTask = nil
            if workspaceActive { activate() }
        }
    }

    func startVM() {
        guard canAct else { return }
        UserDefaults.standard.set(vmCPUs, forKey: "sharedVMCPUs")
        UserDefaults.standard.set(vmMemoryMiB, forKey: "sharedVMMemoryMiB")
        perform("Starting shared Linux VM", success: "Shared Linux VM started.") { [runtime, vmCPUs, vmMemoryMiB] in
            try await runtime.startVM(cpus: vmCPUs, memoryMiB: vmMemoryMiB)
        }
    }

    func startDocker() {
        perform("Starting container engine", success: "Container engine start completed.", ensureVM: true) { [runtime] in
            try await runtime.startDocker()
        }
    }

    func requestStopVM() {
        confirmation = .init(
            title: "Stop the shared Linux VM?",
            message: "This stops the shared kernel and interrupts all containers, Linux machines, and Kubernetes workloads.",
            button: "Stop VM", action: .stopVM
        )
    }

    func requestStopDocker() {
        confirmation = .init(
            title: "Stop the container engine?",
            message: "Running containers and workloads that depend on Docker will be interrupted. Linux machines keep their shared VM.",
            button: "Stop Engine", action: .stopDocker
        )
    }

    func startKubernetes() {
        perform("Starting Kubernetes", success: "Kubernetes start completed. Check the reported status.", ensureVM: true) { [runtime] in
            _ = try await runtime.kubernetes("start")
        }
    }

    func requestStopKubernetes() {
        confirmation = .init(
            title: "Stop Kubernetes?",
            message: "Local cluster workloads will be interrupted.",
            button: "Stop Kubernetes", action: .stopKubernetes
        )
    }

    func requestRemoval(_ record: ResourceRecord) {
        guard record.canDelete else { return }
        let message: String
        let title: String
        switch record {
        case .volume:
            title = "Delete volume \"\(record.name)\"?"
            message = "All data in this volume will be permanently deleted. This cannot be undone."
        case .machine:
            title = "Delete machine \"\(record.name)\"?"
            message = "The machine's Linux filesystem will be permanently deleted. This cannot be undone."
        case .project:
            title = "Bring down \"\(record.name)\"?"
            message = "Compose will stop and remove this project's containers and networks. Named volumes are retained."
        case .container:
            title = "Delete container \"\(record.name)\"?"
            message = "The container's writable filesystem will be deleted. Named volumes and its image are retained."
        case .image:
            title = "Remove \"\(record.name)\"?"
            message = "This local image reference will be removed. Images still required by containers may be refused by Docker."
        case .network:
            title = "Delete network \"\(record.name)\"?"
            message = "The network will be removed. Docker will refuse removal while endpoints are connected."
        }
        confirmation = .init(title: title, message: message, button: {
            if case .project = record { return "Bring Down" }
            return "Delete"
        }(), action: .remove(record))
    }

    func confirm(_ action: ConfirmedAction) {
        switch action {
        case .stopVM:
            perform("Stopping shared Linux VM", success: "Shared Linux VM stop completed.") { [runtime] in
                try await runtime.stopVM()
            }
        case .stopDocker:
            perform("Stopping container engine", success: "Container engine stop completed.") { [runtime] in
                try await runtime.stopDocker()
            }
        case .stopKubernetes:
            perform("Stopping Kubernetes", success: "Kubernetes stop completed.") { [runtime] in
                _ = try await runtime.kubernetes("stop")
            }
        case .remove(let record):
            perform("Removing \(record.name)", success: "\(record.name) removed.") { [runtime] in
                switch record {
                case .container(let item): try await runtime.docker(["container", "rm", "--", item.id])
                case .image(let item): try await runtime.docker(["image", "rm", "--", item.reference])
                case .volume(let item): try await runtime.docker(["volume", "rm", "--", item.name])
                case .network(let item): try await runtime.docker(["network", "rm", "--", item.id])
                case .project(let item):
                    try await runtime.docker(try composeArguments(item) + ["down"], timeout: 180)
                case .machine(let item): try await runtime.deleteMachine(item.name)
                }
            }
        }
    }

    func setRunning(_ record: ResourceRecord, running: Bool) {
        let verb = running ? "Starting" : "Stopping"
        perform("\(verb) \(record.name)", success: "\(record.name): \(running ? "start" : "stop") completed.", ensureVM: running) { [runtime] in
            switch record {
            case .container(let item):
                try await runtime.docker(["container", running ? "start" : "stop", "--", item.id])
            case .machine(let item):
                if running { try await runtime.startMachine(item.name) }
                else { try await runtime.stopMachine(item.name) }
            case .project(let item):
                try await runtime.docker(
                    try composeArguments(item) + (running ? ["up", "--detach"] : ["stop"]), timeout: 300
                )
            default: break
            }
        }
    }

    func restart(_ record: ResourceRecord) {
        guard case .container(let item) = record else { return }
        perform("Restarting \(record.name)", success: "\(record.name) restarted.") { [runtime] in
            try await runtime.docker(["container", "restart", "--", item.id])
        }
    }

    func detailText(for record: ResourceRecord, tab: DetailTab) async throws -> String {
        guard !isPreview else { return PreviewRenderer.detail(for: record, tab: tab) }
        switch tab {
        case .overview: return ""
        case .shell:
            switch record {
            case .container(let item): return try await runtime.terminalCommand(container: item.id)
            case .machine(let item): return try await runtime.terminalCommand(machine: item.name)
            default: return ""
            }
        case .logs:
            switch record {
            case .container(let item): return try await runtime.logs(item.id)
            case .project(let item):
                let result = try await runtime.docker(
                    try composeArguments(item) + ["logs", "--no-color", "--tail", "300"]
                )
                return result.stdout + result.stderr
            default: return ""
            }
        case .inspect:
            let text: String
            switch record {
            case .container(let item): text = try await runtime.inspect("container", id: item.id)
            case .image(let item): text = try await runtime.inspect("image", id: item.imageID)
            case .volume(let item): text = try await runtime.inspect("volume", id: item.name)
            case .network(let item): text = try await runtime.inspect("network", id: item.id)
            case .project(let item):
                text = try await runtime.docker(try composeArguments(item) + ["ps", "--all", "--format", "json"]).stdout
            case .machine(let item):
                let encoder = JSONEncoder()
                encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
                text = String(decoding: try encoder.encode(item), as: UTF8.self)
            }
            return prettyJSON(text)
        }
    }

    func loadKubeconfig() {
        perform("Reading kubeconfig", success: "Kubeconfig received.") { [weak self, runtime] in
            let text = try await runtime.kubernetes("kubeconfig")
            self?.kubeconfig = text
        }
    }

    @Published var kubeconfig: String?

    func copy(_ text: String) {
        guard !isPreview else { return }
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(text, forType: .string)
        successMessage = "Copied to clipboard."
    }

    func saveKubeconfig() {
        guard let kubeconfig, !isPreview else { return }
        let panel = NSSavePanel()
        panel.nameFieldStringValue = "sapphire-kubeconfig.yaml"
        panel.canCreateDirectories = true
        panel.begin { [weak self] response in
            guard response == .OK, let url = panel.url else { return }
            let temporary = url.deletingLastPathComponent().appendingPathComponent(".sapphire-kubeconfig-\(UUID().uuidString)")
            defer { try? FileManager.default.removeItem(at: temporary) }
            do {
                guard FileManager.default.createFile(
                    atPath: temporary.path, contents: Data(kubeconfig.utf8),
                    attributes: [.posixPermissions: 0o600]
                ) else { throw SapphireError.unavailable("Unable to write the kubeconfig file.") }
                if FileManager.default.fileExists(atPath: url.path) {
                    _ = try FileManager.default.replaceItemAt(url, withItemAt: temporary, options: [.usingNewMetadataOnly])
                } else {
                    try FileManager.default.moveItem(at: temporary, to: url)
                }
                self?.successMessage = "Kubeconfig saved."
            } catch {
                self?.issue = AppIssue(title: "Couldn't save kubeconfig", message: error.localizedDescription)
            }
        }
    }
}

func composeArguments(_ project: ComposeProject) throws -> [String] {
    let files = project.configFiles.split(separator: ",").map {
        String($0).trimmingCharacters(in: .whitespacesAndNewlines)
    }.filter { !$0.isEmpty }
    guard !files.isEmpty else {
        throw SapphireError.unavailable("This project did not report configuration files. Open its Compose file to manage it.")
    }
    return ["compose", "--project-name", project.name] + files.flatMap { ["--file", $0] }
}

func prettyJSON(_ text: String) -> String {
    guard let data = text.data(using: .utf8),
          let object = try? JSONSerialization.jsonObject(with: data, options: [.fragmentsAllowed]),
          let pretty = try? JSONSerialization.data(withJSONObject: object, options: [.prettyPrinted, .sortedKeys, .fragmentsAllowed])
    else { return text }
    return String(decoding: pretty, as: UTF8.self)
}

func formatBytes(_ value: UInt64) -> String {
    guard value > 0 else { return "Not reported" }
    return ByteCountFormatter.string(fromByteCount: Int64(clamping: value), countStyle: .binary)
}

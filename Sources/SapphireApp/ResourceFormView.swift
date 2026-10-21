import AppKit
import SapphireCore
import SwiftUI
import UniformTypeIdentifiers

struct ResourceFormView: View {
    @ObservedObject var model: SapphireModel
    let form: ResourceForm
    @Environment(\.dismiss) private var dismiss
    @State private var name = ""
    @State private var image = ""
    @State private var port = ""
    @State private var composePath = ""
    @State private var distribution: LinuxDistribution = .alpine
    @State private var cpus = 2
    @State private var memory = 1
    @State private var vmMemoryMiB = 1024
    @State private var validationError: String?

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 13) {
                SapphireMark(size: 38)
                VStack(alignment: .leading, spacing: 3) {
                    Text(form.title).font(.system(size: 19, weight: .semibold))
                    Text("Your local Sapphire environment").font(.system(size: 11)).foregroundStyle(.secondary)
                }
                Spacer()
            }
            .padding(24)
            Divider()
            ScrollView {
                VStack(alignment: .leading, spacing: 19) {
                    fields
                    if form.needsDocker && !model.snapshot.engineRunning {
                        NoticeView(
                            title: "Container engine is stopped",
                            message: "Start the engine in the sidebar before submitting this form.",
                            symbol: "pause.circle", tint: .orange
                        )
                    }
                    if let validationError {
                        NoticeView(title: "Check your input", message: validationError, symbol: "exclamationmark.circle", tint: .red)
                    }
                }
                .padding(24)
            }
            .frame(maxHeight: 510)
            Divider()
            HStack {
                Text("Actions run against your actual runtime.")
                    .font(.system(size: 10)).foregroundStyle(.tertiary)
                Spacer()
                Button("Cancel") { dismiss() }
                    .keyboardShortcut(.cancelAction)
                Button(form.actionTitle, action: submit)
                    .buttonStyle(.borderedProminent)
                    .keyboardShortcut(.defaultAction)
                    .disabled(!model.canAct || (form.needsDocker && !model.snapshot.engineRunning))
            }
            .padding(18)
        }
        .frame(width: 540)
        .background(SapphireTheme.surface)
        .tint(SapphireTheme.blue)
        .onAppear {
            if case .startVM = form {
                cpus = model.vmCPUs
                vmMemoryMiB = model.vmMemoryMiB
            }
            if case .runContainer(let reference) = form { image = reference }
            if case .cloneMachine(let machine) = form { name = machine.name + "-copy" }
        }
    }

    @ViewBuilder
    private var fields: some View {
        switch form {
        case .startVM:
            VStack(alignment: .leading, spacing: 15) {
                Stepper("CPU ceiling: \(cpus)", value: $cpus, in: 1...64)
                Stepper("Memory ceiling: \(vmMemoryMiB) MiB", value: $vmMemoryMiB, in: 256...262144, step: 256)
            }.font(.system(size: 12))
            NoticeView(
                title: "Resources shared by every workload",
                message: "Containers, machines, and Kubernetes use this one Linux VM. These are configured ceilings, not measured usage. Stop the VM before changing them."
            )
            caption("Small ceilings can prevent workloads from starting. Resource usage and parity with OrbStack have not been benchmarked.")
        case .pullImage:
            field("Image reference", placeholder: "e.g. nginx:alpine", value: $image)
            caption("Use a registry image name, tag, or digest. The download may take a few minutes.")
        case .runContainer:
            field("Container name", placeholder: "e.g. my-web", value: $name)
            field("Image", placeholder: "e.g. nginx:alpine", value: $image)
            field("Publish port (optional)", placeholder: "8080:80", value: $port)
            caption("Use host:container ports. Published ports bind to 127.0.0.1. The image's default command runs detached.")
        case .createVolume:
            field("Volume name", placeholder: "e.g. postgres-data", value: $name)
            caption("Creates a named volume using Docker's local storage driver.")
        case .createNetwork:
            field("Network name", placeholder: "e.g. app-network", value: $name)
            caption("Creates a private Docker bridge network.")
        case .compose:
            VStack(alignment: .leading, spacing: 8) {
                Text("Compose file").font(.system(size: 12, weight: .medium))
                HStack {
                    TextField("/absolute/path/compose.yaml", text: $composePath).textFieldStyle(.roundedBorder)
                    Button("Choose...") { chooseComposeFile() }
                }
            }
            field("Project name (optional)", placeholder: "Use the Compose default", value: $name)
            NoticeView(
                title: "Compose runs in the shared Linux VM",
                message: "Host filesystem sharing is not available in this preview. Compose files that bind mount Mac directories will need guest paths or named volumes."
            )
        case .createMachine:
            field("Machine name", placeholder: "e.g. workspace", value: $name)
            VStack(alignment: .leading, spacing: 8) {
                Text("Distribution").font(.system(size: 12, weight: .medium))
                Picker("Distribution", selection: $distribution) {
                    ForEach([LinuxDistribution.alpine, .ubuntu, .debian, .fedora]) { item in
                        Text(item.title).tag(item)
                    }
                }.labelsHidden().frame(maxWidth: .infinity, alignment: .leading)
            }
            HStack(spacing: 24) {
                VStack(alignment: .leading, spacing: 8) {
                    Text("CPU limit").font(.system(size: 12, weight: .medium))
                    Stepper("\(cpus) \(cpus == 1 ? "CPU" : "CPUs")", value: $cpus, in: 1...64)
                }
                VStack(alignment: .leading, spacing: 8) {
                    Text("Memory limit").font(.system(size: 12, weight: .medium))
                    Stepper("\(memory) GiB", value: $memory, in: 1...256)
                }
            }
            NoticeView(
                title: "A Linux userspace, sharing one kernel",
                message: "Machines use native ARM64 Linux namespaces inside Sapphire's shared VM. These are guest resource limits, not additional VM allocations. Run trusted workloads only: machine root can administer the shared guest and its other workloads. Distribution downloads and first startup may take several minutes."
            )
            caption("Technology preview. Each machine has a capacity-limited sparse disk. Rosetta and Mac home sharing are not available. Resource efficiency has not been benchmarked against OrbStack.")
        case .cloneMachine(let machine):
            Text("Source: \(machine.name)").font(.system(size: 12)).foregroundStyle(.secondary)
            field("New machine name", placeholder: "e.g. workspace-copy", value: $name)
            caption("Creates an independent copy of the machine's guest filesystem. The backend reports any restrictions on cloning.")
        }
    }

    private func field(_ label: String, placeholder: String, value: Binding<String>) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(label).font(.system(size: 12, weight: .medium))
            TextField(placeholder, text: value).textFieldStyle(.roundedBorder)
        }
    }

    private func caption(_ value: String) -> some View {
        Text(value).font(.system(size: 11)).foregroundStyle(.secondary)
            .fixedSize(horizontal: false, vertical: true)
    }

    private func chooseComposeFile() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = false
        panel.allowsMultipleSelection = false
        panel.message = "Choose a Docker Compose YAML file."
        panel.begin { response in
            if response == .OK, let url = panel.url { composePath = url.path }
        }
    }

    private func submit() {
        let trimmedName = name.trimmingCharacters(in: .whitespacesAndNewlines)
        let trimmedImage = image.trimmingCharacters(in: .whitespacesAndNewlines)
        let runtime = model.runtime
        do {
            switch form {
            case .startVM:
                model.vmCPUs = cpus
                model.vmMemoryMiB = vmMemoryMiB
                model.startVM()
            case .pullImage:
                try validateImage(trimmedImage)
                model.perform("Pulling \(trimmedImage)", success: "Image pulled: \(trimmedImage).") {
                    try await runtime.docker(["image", "pull", "--", trimmedImage], timeout: 900)
                }
            case .runContainer:
                try ResourceName.validate(trimmedName)
                try validateImage(trimmedImage)
                var arguments = ["container", "run", "--detach", "--name", trimmedName]
                let binding = port.trimmingCharacters(in: .whitespacesAndNewlines)
                if !binding.isEmpty {
                    let parts = binding.split(separator: ":", omittingEmptySubsequences: false)
                    guard parts.count == 2, parts.allSatisfy({
                        guard let number = Int($0) else { return false }
                        return (1...65535).contains(number) && $0.allSatisfy(\.isNumber)
                    }) else { throw SapphireError.invalidInput("Enter ports as host:container, with each port from 1 to 65535.") }
                    arguments += ["--publish", "127.0.0.1:\(binding)"]
                }
                arguments += ["--", trimmedImage]
                let command = arguments
                model.perform("Starting \(trimmedName)", success: "Container created: \(trimmedName).") {
                    try await runtime.docker(command, timeout: 900)
                }
            case .createVolume:
                try ResourceName.validate(trimmedName)
                model.perform("Creating volume", success: "Volume created: \(trimmedName).") {
                    try await runtime.docker(["volume", "create", "--", trimmedName])
                }
            case .createNetwork:
                try ResourceName.validate(trimmedName)
                model.perform("Creating network", success: "Network created: \(trimmedName).") {
                    try await runtime.docker(["network", "create", "--driver", "bridge", "--", trimmedName])
                }
            case .compose:
                let expanded = NSString(string: composePath.trimmingCharacters(in: .whitespacesAndNewlines)).expandingTildeInPath
                guard expanded.hasPrefix("/"), FileManager.default.isReadableFile(atPath: expanded) else {
                    throw SapphireError.invalidInput("Choose a readable Compose file using an absolute path.")
                }
                if !trimmedName.isEmpty { try ResourceName.validate(trimmedName) }
                var arguments = ["compose", "--file", expanded]
                if !trimmedName.isEmpty { arguments += ["--project-name", trimmedName] }
                arguments += ["up", "--detach"]
                let command = arguments
                let directory = URL(fileURLWithPath: expanded).deletingLastPathComponent()
                model.perform("Starting Compose project", success: "Compose project brought up.") {
                    try await runtime.docker(command, timeout: 900, directory: directory)
                }
            case .createMachine:
                let config = MachineConfiguration(
                    name: trimmedName, distribution: distribution,
                    cpus: cpus, memoryGiB: memory, diskGiB: 16
                )
                try config.validate()
                model.perform("Creating \(trimmedName)", success: "Machine created: \(trimmedName).", ensureVM: true) {
                    try await runtime.createMachine(config, start: true)
                }
            case .cloneMachine(let source):
                try ResourceName.validate(trimmedName)
                guard trimmedName != "docker", trimmedName != source.name else {
                    throw SapphireError.invalidInput("Choose a different name. The name 'docker' is reserved.")
                }
                model.perform("Cloning \(source.name)", success: "Machine cloned to \(trimmedName).") {
                    try await runtime.cloneMachine(source.name, to: trimmedName)
                }
            }
            dismiss()
        } catch {
            validationError = error.localizedDescription
        }
    }

    private func validateImage(_ reference: String) throws {
        guard !reference.isEmpty, !reference.hasPrefix("-"), reference.utf8.count <= 512,
              !reference.contains(where: { $0.isWhitespace || $0.isNewline }),
              !reference.contains("\0") else {
            throw SapphireError.invalidInput("Enter an image name, optionally with a registry, tag, or digest.")
        }
    }
}

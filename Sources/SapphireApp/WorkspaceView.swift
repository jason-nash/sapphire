import SapphireCore
import SwiftUI

struct WorkspaceView: View {
    @ObservedObject var model: SapphireModel
    @Environment(\.scenePhase) private var scenePhase

    var body: some View {
        HStack(spacing: 0) {
            sidebar
            Rectangle().fill(SapphireTheme.line).frame(width: 1)
            VStack(spacing: 0) {
                windowHeader
                Divider()
                content
                footer
            }
            .background(SapphireTheme.surface)
        }
        .frame(minWidth: 1080, minHeight: 700)
        .background(SapphireTheme.canvas)
        .tint(SapphireTheme.blue)
        .sheet(item: $model.presentedForm) { form in
            ResourceFormView(model: model, form: form)
        }
        .sheet(isPresented: $model.showAbout) { MaturityView() }
        .alert(
            model.confirmation?.title ?? "Confirm action",
            isPresented: Binding(
                get: { model.confirmation != nil },
                set: { if !$0 { model.confirmation = nil } }
            ),
            presenting: model.confirmation
        ) { item in
            Button(item.button, role: .destructive) { model.confirm(item.action) }
            Button("Cancel", role: .cancel) {}
        } message: { item in
            Text(item.message)
        }
        .onAppear { model.activate() }
        .onDisappear { model.cancelRefresh() }
        .onChange(of: scenePhase) { _, phase in
            if phase == .active { model.activate() }
            else if phase == .background { model.cancelRefresh() }
        }
        .onChange(of: model.search) { _, _ in model.reconcileSelection() }
        .task(id: model.section) {
            if model.section == .kubernetes { await model.refreshKubernetes() }
        }
    }

    private var sidebar: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 10) {
                SapphireMark(size: 33)
                VStack(alignment: .leading, spacing: 2) {
                    Text("Sapphire").font(.system(size: 19, weight: .semibold, design: .rounded))
                    Text("A little space to build.").font(.system(size: 9)).foregroundStyle(.secondary)
                }
            }
            .padding(.horizontal, 20)
            .padding(.top, 42)
            .padding(.bottom, 31)

            Text("WORKSPACE")
                .font(.system(size: 9, weight: .semibold))
                .tracking(1.4).foregroundStyle(.tertiary)
                .padding(.leading, 23).padding(.bottom, 10)

            VStack(spacing: 3) {
                ForEach(WorkspaceSection.allCases) { section in
                    Button { model.navigate(to: section) } label: {
                        HStack(spacing: 10) {
                            Image(systemName: section.symbol)
                                .font(.system(size: 14, weight: .medium))
                                .frame(width: 21)
                            Text(section.title).font(.system(size: 12, weight: model.section == section ? .semibold : .regular))
                            Spacer(minLength: 0)
                            if section != .kubernetes {
                                Text("\(model.records(in: section).count)")
                                    .font(.system(size: 10, weight: .medium, design: .monospaced))
                                    .foregroundStyle(model.section == section ? SapphireTheme.blue : SapphireTheme.secondary)
                            }
                        }
                        .foregroundStyle(model.section == section ? SapphireTheme.blue : SapphireTheme.ink.opacity(0.72))
                        .padding(.horizontal, 11).padding(.vertical, 10)
                        .background(
                            model.section == section ? SapphireTheme.blue.opacity(0.095) : .clear,
                            in: RoundedRectangle(cornerRadius: 8)
                        )
                        .contentShape(RoundedRectangle(cornerRadius: 8))
                    }
                    .buttonStyle(.plain)
                    .accessibilityAddTraits(model.section == section ? .isSelected : [])
                }
            }
            .padding(.horizontal, 12)

            Spacer(minLength: 24)
            runtimeCard.padding(.horizontal, 14)
            Button { model.showAbout = true } label: {
                HStack(spacing: 7) {
                    Image(systemName: "sparkle")
                    Text("Technology preview")
                    Spacer()
                    Image(systemName: "info.circle")
                }
                .font(.system(size: 10)).foregroundStyle(.secondary)
                .padding(.horizontal, 20).padding(.vertical, 17)
            }.buttonStyle(.plain)
        }
        .frame(width: 216)
        .background(LinearGradient(
            colors: [SapphireTheme.canvas, SapphireTheme.blue.opacity(0.045)],
            startPoint: .topLeading, endPoint: .bottomTrailing
        ))
    }

    private var runtimeCard: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                Image(systemName: "cpu").foregroundStyle(SapphireTheme.blue)
                Text("Shared Linux VM").font(.system(size: 11, weight: .semibold))
                Spacer(minLength: 0)
                Circle().fill(model.snapshot.virtualMachine.running ? SapphireTheme.green : Color.secondary.opacity(0.4))
                    .frame(width: 6, height: 6)
            }
            if model.snapshot.virtualMachine.running {
                Text("\(model.snapshot.virtualMachine.cpus) CPUs  /  \(model.snapshot.virtualMachine.memoryMiB) MiB configured")
                    .font(.system(size: 9, design: .monospaced)).foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            } else {
                Text(model.hasLoaded ? "Stopped" : "Checking runtime...")
                    .font(.system(size: 10)).foregroundStyle(.secondary)
            }
            HStack(spacing: 6) {
                Text("Docker").font(.system(size: 10)).foregroundStyle(.secondary)
                Spacer()
                Text(model.snapshot.engineRunning ? "Running" : "Stopped")
                    .font(.system(size: 10, weight: .medium))
                    .foregroundStyle(model.snapshot.engineRunning ? SapphireTheme.green : SapphireTheme.secondary)
            }
            HStack(spacing: 7) {
                Button {
                    if model.snapshot.engineRunning { model.requestStopDocker() }
                    else { model.startDocker() }
                } label: {
                    Label(
                        model.snapshot.engineRunning ? "Stop Engine" : "Start Engine",
                        systemImage: model.snapshot.engineRunning ? "stop.fill" : "play.fill"
                    ).font(.system(size: 10, weight: .medium))
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.bordered)
                .controlSize(.small)
                .disabled(!model.canAct || (!model.snapshot.engineRunning && !model.canStartDocker))
                Menu {
                    Button("Configure & Start VM...") { model.presentedForm = .startVM }
                        .disabled(model.snapshot.virtualMachine.running || !model.canStartVM)
                    Button("Stop Shared VM...", role: .destructive) { model.requestStopVM() }
                        .disabled(!model.snapshot.virtualMachine.running)
                } label: {
                    Image(systemName: "ellipsis")
                }
                .menuStyle(.borderlessButton)
                .frame(width: 16)
                .disabled(!model.canAct)
                .help("Shared VM controls")
            }
        }
        .padding(13)
        .background(SapphireTheme.surface.opacity(0.8), in: RoundedRectangle(cornerRadius: 11))
        .overlay(RoundedRectangle(cornerRadius: 11).stroke(SapphireTheme.line))
    }

    private var windowHeader: some View {
        HStack(spacing: 10) {
            Image(systemName: "circle.grid.2x2.fill")
                .font(.system(size: 11)).foregroundStyle(SapphireTheme.blue.opacity(0.7))
            Text("Local workspace").font(.system(size: 11, weight: .medium)).foregroundStyle(.secondary)
            Image(systemName: "chevron.right").font(.system(size: 8, weight: .semibold)).foregroundStyle(.tertiary)
            Text(model.section.title).font(.system(size: 11, weight: .medium))
            Spacer()
            if model.isPreview {
                Text("PREVIEW DATA").font(.system(size: 9, weight: .semibold)).tracking(1)
                    .foregroundStyle(SapphireTheme.blue)
            } else {
                StatusBadge(
                    title: model.snapshot.virtualMachine.running ? "Linux online" : (model.hasLoaded ? "Linux offline" : "Connecting"),
                    active: model.snapshot.virtualMachine.running
                )
            }
            Button { model.activate() } label: {
                Image(systemName: "arrow.clockwise").font(.system(size: 13))
                    .frame(width: 28, height: 28)
            }
            .buttonStyle(.plain)
            .help("Refresh (Command-R)")
            .keyboardShortcut("r", modifiers: .command)
            .disabled(!model.canAct)
            .accessibilityLabel("Refresh workspace")
        }
        .padding(.horizontal, 26)
        .frame(height: 59)
    }

    private var content: some View {
        VStack(alignment: .leading, spacing: 20) {
            HStack(alignment: .center) {
                VStack(alignment: .leading, spacing: 6) {
                    Text(model.section.title).font(.system(size: 27, weight: .semibold, design: .rounded))
                    Text(model.section.subtitle).font(.system(size: 12)).foregroundStyle(.secondary)
                }
                Spacer()
                if model.section != .kubernetes {
                    Button { model.presentCreationForm() } label: {
                        Label(model.section.addTitle, systemImage: "plus").font(.system(size: 11, weight: .semibold))
                            .padding(.vertical, 3)
                    }
                    .buttonStyle(.borderedProminent)
                    .disabled(!model.canAct || !canCreate)
                }
            }

            if let issue = model.issue {
                HStack(alignment: .top, spacing: 7) {
                    NoticeView(title: issue.title, message: issue.message, symbol: "exclamationmark.triangle", tint: .red)
                    Button { model.issue = nil } label: { Image(systemName: "xmark").font(.system(size: 10)) }
                        .buttonStyle(.plain).padding(.top, 12).help("Dismiss error")
                }
            }
            if model.dependencies?.ready == false {
                dependencyNotice
            }
            if !model.snapshot.warnings.isEmpty {
                DisclosureGroup {
                    Text(model.snapshot.warnings.joined(separator: "\n\n"))
                        .font(.system(size: 11)).foregroundStyle(.secondary)
                        .textSelection(.enabled).padding(.top, 7)
                } label: {
                    Label("Some runtime data is unavailable (\(model.snapshot.warnings.count))", systemImage: "exclamationmark.triangle")
                        .font(.system(size: 11)).foregroundStyle(.orange)
                }
            }

            if model.section == .kubernetes {
                KubernetesView(model: model)
            } else {
                resourceSummary
                resourceBrowser
            }
        }
        .padding(26)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }

    private var canCreate: Bool {
        model.section == .machines ? model.canStartVM : model.snapshot.engineRunning
    }

    private var dependencyNotice: some View {
        let missing = [
            model.dependencies?.engine == nil ? "sapphire-engine" : nil,
            model.dependencies?.docker == nil ? "Docker CLI" : nil,
            model.dependencies?.guestAssets == false ? "guest kernel, initrd, or root filesystem" : nil,
        ].compactMap { $0 }.joined(separator: ", ")
        return NoticeView(
            title: "Runtime setup needed",
            message: "Missing: \(missing). Build the runtime tools and provision the guest assets before starting. Open About Sapphire for the current implementation limits.",
            symbol: "wrench.and.screwdriver", tint: .orange
        )
    }

    private var resourceSummary: some View {
        HStack(spacing: 12) {
            summaryCard(
                title: model.section == .containers ? "CONTAINERS" : "LOCAL RESOURCES",
                value: "\(model.records(in: model.section).count)",
                note: model.section == .containers ? "in your workspace" : model.section.title.lowercased(),
                symbol: model.section.symbol
            )
            summaryCard(
                title: model.section == .containers || model.section == .machines || model.section == .compose ? "RUNNING" : "CONTAINER ENGINE",
                value: model.section == .containers || model.section == .machines || model.section == .compose
                    ? "\(model.records(in: model.section).filter(\.isRunning).count)"
                    : (model.snapshot.engineRunning ? "Online" : "Offline"),
                note: model.section == .machines ? "Linux userspaces" : "inside shared Linux",
                symbol: "bolt.fill"
            )
            summaryCard(
                title: "ONE SHARED KERNEL",
                value: model.snapshot.virtualMachine.running ? "Online" : "Offline",
                note: "Hypervisor.framework",
                symbol: "cpu"
            )
        }
        .frame(height: 98)
    }

    private func summaryCard(title: String, value: String, note: String, symbol: String) -> some View {
        HStack(alignment: .top) {
            VStack(alignment: .leading, spacing: 7) {
                Text(title).font(.system(size: 8, weight: .semibold)).tracking(1.0).foregroundStyle(.secondary)
                Text(value).font(.system(size: 24, weight: .medium, design: .rounded))
                Text(note).font(.system(size: 10)).foregroundStyle(.tertiary)
            }
            Spacer(minLength: 4)
            Image(systemName: symbol).font(.system(size: 14)).foregroundStyle(SapphireTheme.blue.opacity(0.65))
                .padding(8).background(SapphireTheme.blue.opacity(0.055), in: RoundedRectangle(cornerRadius: 8))
        }
        .padding(16)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .leading)
        .background(LinearGradient(
            colors: [SapphireTheme.blue.opacity(0.025), SapphireTheme.canvas.opacity(0.35)],
            startPoint: .topLeading, endPoint: .bottomTrailing
        ), in: RoundedRectangle(cornerRadius: 11))
        .overlay(RoundedRectangle(cornerRadius: 11).stroke(SapphireTheme.line))
    }

    private var resourceBrowser: some View {
        VStack(spacing: 0) {
            HStack {
                HStack(spacing: 7) {
                    Image(systemName: "magnifyingglass").foregroundStyle(.tertiary)
                    TextField("Search \(model.section.title.lowercased())", text: $model.search)
                        .textFieldStyle(.plain).font(.system(size: 11))
                        .accessibilityLabel("Search \(model.section.title)")
                    if !model.search.isEmpty {
                        Button { model.search = "" } label: {
                            Image(systemName: "xmark.circle.fill").foregroundStyle(.tertiary)
                        }.buttonStyle(.plain).help("Clear search")
                    }
                }
                .padding(8).frame(maxWidth: 310)
                .background(Color.primary.opacity(0.035), in: RoundedRectangle(cornerRadius: 7))
                Spacer()
                Text("\(model.resources.count) \(model.resources.count == 1 ? "item" : "items")")
                    .font(.system(size: 10)).foregroundStyle(.tertiary)
            }.padding(12)
            Divider()
            HSplitView {
                resourceList.frame(minWidth: 310, idealWidth: 390)
                Group {
                    if let resource = model.selectedResource {
                        ResourceDetailView(model: model, resource: resource).id(resource.id)
                    } else {
                        VStack(spacing: 10) {
                            Image(systemName: "sidebar.right").font(.system(size: 25, weight: .light)).foregroundStyle(.quaternary)
                            Text("Select a resource").font(.system(size: 12)).foregroundStyle(.tertiary)
                        }.frame(maxWidth: .infinity, maxHeight: .infinity)
                    }
                }
                .frame(minWidth: 310, idealWidth: 360)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(SapphireTheme.surface)
        .clipShape(RoundedRectangle(cornerRadius: 11))
        .overlay(RoundedRectangle(cornerRadius: 11).stroke(SapphireTheme.line))
    }

    private var resourceList: some View {
        VStack(spacing: 0) {
            HStack {
                Text("NAME / DETAILS")
                Spacer()
                Text("STATUS")
            }
            .font(.system(size: 8, weight: .semibold)).tracking(0.8).foregroundStyle(.tertiary)
            .padding(.horizontal, 18).padding(.vertical, 12)

            if model.resources.isEmpty {
                emptyState
            } else {
                ScrollView {
                    LazyVStack(spacing: 2) {
                        ForEach(model.resources) { resource in
                            resourceRow(resource)
                        }
                    }.padding(.horizontal, 8).padding(.bottom, 8)
                }
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    private func resourceRow(_ resource: ResourceRecord) -> some View {
        Button { model.selectedID = resource.id } label: {
            HStack(spacing: 11) {
                ResourceIcon(symbol: resource.symbol, active: resource.isRunning || model.selectedID == resource.id, size: 33)
                VStack(alignment: .leading, spacing: 5) {
                    Text(resource.name).font(.system(size: 11, weight: .semibold))
                        .foregroundStyle(SapphireTheme.ink).lineLimit(1)
                    Text(resource.subtitle).font(.system(size: 9, design: .monospaced))
                        .foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle)
                }
                Spacer(minLength: 4)
                StatusBadge(title: resource.status, active: resource.isRunning, neutral: !resource.hasLifecycle)
            }
            .padding(.horizontal, 10).padding(.vertical, 12)
            .background(
                model.selectedID == resource.id ? SapphireTheme.blue.opacity(0.065) : .clear,
                in: RoundedRectangle(cornerRadius: 8)
            )
            .overlay(alignment: .leading) {
                if model.selectedID == resource.id {
                    Capsule().fill(SapphireTheme.blue).frame(width: 3, height: 23).padding(.leading, 1)
                }
            }
            .contentShape(RoundedRectangle(cornerRadius: 8))
        }
        .buttonStyle(.plain)
        .accessibilityAddTraits(model.selectedID == resource.id ? .isSelected : [])
        .contextMenu { ResourceActions(model: model, resource: resource) }
    }

    private var emptyState: some View {
        VStack(spacing: 13) {
            if model.isRefreshing && !model.hasLoaded {
                ProgressView().controlSize(.small)
                Text("Reading your workspace...").font(.system(size: 12)).foregroundStyle(.secondary)
            } else {
                ResourceIcon(symbol: model.section.symbol, size: 50)
                Text(model.search.isEmpty ? "No \(model.section.title.lowercased()) yet" : "No matching resources")
                    .font(.system(size: 13, weight: .medium))
                Text(emptyMessage).font(.system(size: 11)).foregroundStyle(.secondary)
                    .multilineTextAlignment(.center).frame(maxWidth: 240)
                if model.search.isEmpty {
                    Button(model.section.addTitle) { model.presentCreationForm() }
                        .buttonStyle(.bordered).controlSize(.small)
                        .disabled(!model.canAct || !canCreate)
                }
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .padding(24)
    }

    private var emptyMessage: String {
        if !model.search.isEmpty { return "Try a different name, image, or status." }
        if !model.snapshot.virtualMachine.running { return "Start the shared Linux VM to load your workspace." }
        if !model.snapshot.engineRunning && model.section != .machines { return "Start the container engine to load Docker resources." }
        return "Create your first resource to get started."
    }

    private var footer: some View {
        HStack(spacing: 7) {
            if let title = model.operationTitle {
                ProgressView().controlSize(.mini)
                Text(title + "...").font(.system(size: 10))
            } else if model.isRefreshing {
                ProgressView().controlSize(.mini)
                Text("Refreshing workspace...").font(.system(size: 10))
            } else if let message = model.successMessage {
                Image(systemName: "checkmark.circle").foregroundStyle(SapphireTheme.green)
                Text(message).font(.system(size: 10)).lineLimit(1)
            } else {
                Circle().fill(SapphireTheme.green.opacity(0.75)).frame(width: 5, height: 5)
                Text(model.isPreview ? "Offscreen preview / illustrative data" : "Refreshes on demand. No background polling.")
                    .font(.system(size: 10)).foregroundStyle(.secondary)
            }
            Spacer()
            if let date = model.lastUpdated {
                Text("Updated \(date.formatted(date: .omitted, time: .shortened))")
                    .font(.system(size: 9)).foregroundStyle(.tertiary)
            }
        }
        .padding(.horizontal, 26).frame(height: 34)
        .background(SapphireTheme.canvas.opacity(0.4))
    }
}

struct ResourceActions: View {
    @ObservedObject var model: SapphireModel
    let resource: ResourceRecord

    var body: some View {
        if resource.hasLifecycle {
            Button(resource.isRunning ? "Stop" : "Start") {
                model.setRunning(resource, running: !resource.isRunning)
            }.disabled(!model.canAct || !lifecycleAvailable)
        }
        if case .container = resource, resource.isRunning {
            Button("Restart") { model.restart(resource) }.disabled(!model.canAct)
        }
        if case .image(let image) = resource {
            Button("Run Container...") { model.presentedForm = .runContainer(image: image.reference) }
                .disabled(!model.canAct || !model.snapshot.engineRunning)
        }
        if case .machine(let machine) = resource {
            Button("Clone...") { model.presentedForm = .cloneMachine(machine) }
                .disabled(!model.canAct || !model.snapshot.virtualMachine.running)
        }
        Divider()
        Button(removalTitle, role: .destructive) { model.requestRemoval(resource) }
            .disabled(!model.canAct || !resource.canDelete || !removalAvailable)
    }

    private var lifecycleAvailable: Bool {
        if case .machine = resource { return model.canStartVM }
        return model.snapshot.engineRunning
    }

    private var removalAvailable: Bool {
        if case .machine = resource { return model.snapshot.virtualMachine.running }
        return model.snapshot.engineRunning
    }

    private var removalTitle: String {
        if case .project = resource { return "Bring Down..." }
        return "Delete..."
    }
}

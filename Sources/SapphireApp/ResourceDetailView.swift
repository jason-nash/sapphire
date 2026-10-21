import SapphireCore
import SwiftUI

enum DetailTab: String, CaseIterable, Identifiable {
    case overview = "Overview"
    case logs = "Logs"
    case inspect = "Inspect"
    case shell = "Shell"
    var id: String { rawValue }
}

struct ResourceDetailView: View {
    @ObservedObject var model: SapphireModel
    let resource: ResourceRecord
    @State private var tab: DetailTab = .overview
    @State private var output = ""
    @State private var error: String?
    @State private var loading = false
    @State private var reloadID = 0
    @State private var outputRequest = UUID()

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            header.padding(18)
            HStack(spacing: 17) {
                ForEach(tabs) { item in
                    Button { tab = item } label: {
                        VStack(spacing: 10) {
                            Text(item.rawValue).font(.system(size: 10, weight: tab == item ? .semibold : .regular))
                                .foregroundStyle(tab == item ? SapphireTheme.blue : SapphireTheme.secondary)
                            Rectangle().fill(tab == item ? SapphireTheme.blue : .clear).frame(height: 2)
                        }
                    }.buttonStyle(.plain)
                }
                Spacer(minLength: 0)
            }
            .padding(.horizontal, 18)
            Divider()
            if tab == .overview {
                overview
            } else {
                outputView.padding(16)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        .task(id: "\(resource.id)-\(tab.rawValue)-\(model.detailRevision)-\(reloadID)-\(model.isRefreshing)-\(model.isBusy)") {
            await loadOutput()
        }
    }

    private var tabs: [DetailTab] {
        [.overview] + (resource.supportsLogs ? [.logs] : []) + [.inspect] + (resource.supportsShell ? [.shell] : [])
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack(alignment: .top, spacing: 11) {
                ResourceIcon(symbol: resource.symbol, active: resource.isRunning, size: 40)
                VStack(alignment: .leading, spacing: 5) {
                    Text(resource.name).font(.system(size: 15, weight: .semibold))
                        .lineLimit(2).textSelection(.enabled)
                    StatusBadge(title: resource.status, active: resource.isRunning, neutral: !resource.hasLifecycle)
                }
                Spacer(minLength: 0)
                Menu { ResourceActions(model: model, resource: resource) } label: {
                    Image(systemName: "ellipsis").frame(width: 18, height: 18)
                }.menuStyle(.borderlessButton).frame(width: 22).help("Resource actions")
            }
            HStack(spacing: 8) {
                if resource.hasLifecycle {
                    Button {
                        model.setRunning(resource, running: !resource.isRunning)
                    } label: {
                        Label(resource.isRunning ? "Stop" : "Start", systemImage: resource.isRunning ? "stop.fill" : "play.fill")
                    }
                    .buttonStyle(QuietButtonStyle())
                    .disabled(!model.canAct || !lifecycleAvailable)
                }
                if case .image(let image) = resource {
                    Button {
                        model.presentedForm = .runContainer(image: image.reference)
                    } label: {
                        Label("Run", systemImage: "play.fill")
                    }
                    .buttonStyle(QuietButtonStyle())
                    .disabled(!model.canAct || !model.snapshot.engineRunning)
                }
                if resource.supportsShell {
                    Button { tab = .shell } label: {
                        Label("Shell", systemImage: "terminal")
                    }
                    .buttonStyle(QuietButtonStyle())
                }
            }
        }
    }

    private var lifecycleAvailable: Bool {
        if case .machine = resource { return model.canStartVM }
        return model.snapshot.engineRunning
    }

    private var overview: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 19) {
                Text("RESOURCE DETAILS").font(.system(size: 8, weight: .semibold)).tracking(1.0).foregroundStyle(.tertiary)
                ForEach(resource.fields) { field in
                    VStack(alignment: .leading, spacing: 6) {
                        Text(field.label).font(.system(size: 10)).foregroundStyle(.secondary)
                        Text(field.value).font(.system(size: 11, design: .monospaced))
                            .foregroundStyle(SapphireTheme.ink.opacity(0.85))
                            .textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
                    }
                }
                if case .container = resource {
                    HStack(spacing: 7) {
                        Image(systemName: "doc.text").foregroundStyle(SapphireTheme.blue)
                        Text("Logs and inspect are fetched when opened.")
                            .font(.system(size: 10)).foregroundStyle(.secondary)
                    }
                    .padding(11)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(SapphireTheme.blue.opacity(0.04), in: RoundedRectangle(cornerRadius: 8))
                }
                if case .machine = resource {
                    NoticeView(
                        title: "Shared kernel architecture",
                        message: "This machine shares the Linux kernel and has its own capacity-limited sparse disk. Interactive terminals are unavailable. Use sapphire exec \(resource.name) -- COMMAND for noninteractive commands."
                    )
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(18)
        }
    }

    private var outputView: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                Text(tab == .logs ? "LAST 300 LINES" : (tab == .shell ? "TERMINAL COMMAND" : "RUNTIME OUTPUT"))
                    .font(.system(size: 8, weight: .semibold)).tracking(0.8).foregroundStyle(.tertiary)
                Spacer()
                Button { model.copy(output) } label: { Image(systemName: "doc.on.doc") }
                    .buttonStyle(.plain).help("Copy output").disabled(output.isEmpty || loading || model.isPreview)
                Button { reloadID += 1 } label: { Image(systemName: "arrow.clockwise") }
                    .buttonStyle(.plain).help("Reload detail")
                    .disabled(loading || !model.canAct)
            }
            if tab == .shell {
                Text("Copy this command into your terminal. Sapphire uses its own runtime and shell quoting.")
                    .font(.system(size: 11)).foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                if !resource.isRunning {
                    Text("Start this container before running the command.")
                        .font(.system(size: 11)).foregroundStyle(.orange)
                }
            }
            if loading {
                VStack(spacing: 10) {
                    ProgressView().controlSize(.small)
                    Text("Reading \(tab.rawValue.lowercased())...").font(.system(size: 11)).foregroundStyle(.secondary)
                }.frame(maxWidth: .infinity, maxHeight: .infinity)
            } else if let error {
                NoticeView(title: "Couldn't load \(tab.rawValue.lowercased())", message: error, symbol: "exclamationmark.triangle", tint: .red)
                Spacer()
            } else {
                CodePanel(text: output)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }

    private func loadOutput() async {
        let request = UUID()
        outputRequest = request
        guard tab != .overview else { return }
        guard !model.isBusy, !model.isRefreshing else {
            loading = false
            output = "A runtime operation is in progress. Reload after it completes."
            return
        }
        loading = true
        error = nil
        output = ""
        defer { if outputRequest == request { loading = false } }
        do {
            let text = try await model.detailText(for: resource, tab: tab)
            try Task.checkCancellation()
            guard outputRequest == request else { return }
            output = text
        } catch is CancellationError {
        } catch {
            if outputRequest == request { self.error = error.localizedDescription }
        }
    }
}

import SwiftUI

private struct ClusterReport {
    let running: Bool
    let ready: Bool
    let available: Bool
    let version: String
    let error: String?

    init?(text: String?) {
        guard let text, let data = text.data(using: .utf8),
              let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else { return nil }
        let result = object["result"] as? [String: Any] ?? object
        guard let running = result["running"] as? Bool else { return nil }
        self.running = running
        ready = result["ready"] as? Bool ?? false
        available = result["available"] as? Bool ?? false
        version = result["version"] as? String ?? "Not reported"
        error = result["error"] as? String
    }
}

struct KubernetesView: View {
    @ObservedObject var model: SapphireModel

    private var report: ClusterReport? { ClusterReport(text: model.kubernetesStatus) }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 20) {
                HStack(alignment: .top, spacing: 20) {
                    Image(systemName: "helm")
                        .font(.system(size: 42, weight: .light))
                        .foregroundStyle(SapphireTheme.blue)
                        .frame(width: 86, height: 86)
                        .background(SapphireTheme.blue.opacity(0.07), in: RoundedRectangle(cornerRadius: 21))
                    VStack(alignment: .leading, spacing: 9) {
                        HStack {
                            Text("Your local cluster").font(.system(size: 20, weight: .semibold, design: .rounded))
                            StatusBadge(title: clusterTitle, active: report?.ready == true)
                        }
                        Text("K3s shares Sapphire's Linux VM and Docker engine.")
                            .font(.system(size: 12)).foregroundStyle(.secondary)
                        HStack(spacing: 9) {
                            if report?.running == true {
                                Button("Stop Kubernetes...") { model.requestStopKubernetes() }
                                    .buttonStyle(.bordered)
                                    .disabled(!model.canAct || model.isLoadingKubernetes)
                            } else {
                                Button("Start Kubernetes") { model.startKubernetes() }
                                    .buttonStyle(.borderedProminent)
                                    .disabled(!model.canAct || !model.canStartDocker || model.isLoadingKubernetes || report?.available == false)
                            }
                            Button("Read Kubeconfig") { model.loadKubeconfig() }
                                .buttonStyle(.bordered)
                                .disabled(!model.canAct || report?.ready != true)
                            Button {
                                model.activate()
                            } label: {
                                Image(systemName: "arrow.clockwise")
                            }
                            .buttonStyle(.bordered).help("Refresh Kubernetes status")
                            .disabled(!model.canAct || !model.snapshot.virtualMachine.running || model.isLoadingKubernetes)
                        }.padding(.top, 5)
                    }
                    Spacer(minLength: 0)
                }
                .padding(24)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(LinearGradient(
                    colors: [SapphireTheme.blue.opacity(0.025), SapphireTheme.canvas.opacity(0.4)],
                    startPoint: .topLeading, endPoint: .bottomTrailing
                ), in: RoundedRectangle(cornerRadius: 12))
                .overlay(RoundedRectangle(cornerRadius: 12).stroke(SapphireTheme.line))

                if let error = model.kubernetesError ?? report?.error {
                    NoticeView(title: "Kubernetes needs attention", message: error, symbol: "exclamationmark.triangle", tint: .orange)
                }

                NoticeView(
                    title: "Technology preview",
                    message: "Kubernetes requires the guest's K3s assets. Startup, networking, and kubeconfig connectivity depend on the current runtime. Resource efficiency and feature parity with OrbStack have not been established."
                )

                HStack {
                    Text("REPORTED CLUSTER STATUS").font(.system(size: 9, weight: .semibold)).tracking(1).foregroundStyle(.secondary)
                    Spacer()
                    if let report {
                        Text(report.version).font(.system(size: 10, design: .monospaced)).foregroundStyle(.tertiary)
                    }
                    if model.isLoadingKubernetes { ProgressView().controlSize(.mini) }
                }
                CodePanel(
                    text: model.kubernetesStatus ?? (model.snapshot.virtualMachine.running
                        ? "No status received. Use Refresh to request cluster status."
                        : "The shared Linux VM is stopped."),
                    minHeight: 190
                ).frame(height: 230)

                if let config = model.kubeconfig {
                    HStack {
                        Text("KUBECONFIG").font(.system(size: 9, weight: .semibold)).tracking(1).foregroundStyle(.secondary)
                        Spacer()
                        Button("Copy") { model.copy(config) }.disabled(model.isPreview)
                        Button("Save...") { model.saveKubeconfig() }.disabled(model.isPreview)
                    }
                    Text("Contains cluster credentials. The endpoint is returned by the runtime; check host connectivity before using it.")
                        .font(.system(size: 11)).foregroundStyle(.secondary)
                    CodePanel(text: config, minHeight: 160).frame(height: 200)
                }
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    private var clusterTitle: String {
        if model.isLoadingKubernetes { return "Checking" }
        if !model.snapshot.virtualMachine.running { return "VM stopped" }
        guard let report else { return "Unknown" }
        if report.ready { return "Ready" }
        if report.running { return "Not ready" }
        return report.available ? "Stopped" : "Unavailable"
    }
}

struct MaturityView: View {
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(alignment: .leading, spacing: 22) {
            HStack(spacing: 16) {
                SapphireMark(size: 58)
                VStack(alignment: .leading, spacing: 5) {
                    Text("Sapphire").font(.system(size: 28, weight: .semibold, design: .rounded))
                    Text("Open source. Native to your Mac.").font(.system(size: 12)).foregroundStyle(.secondary)
                }
            }
            NoticeView(
                title: "An early technology preview",
                message: "Sapphire is building its own Hypervisor.framework VMM, with containers, Linux namespaces, and K3s sharing one guest kernel. The desktop app reflects data and errors returned by that runtime."
            )
            VStack(alignment: .leading, spacing: 13) {
                maturityLine("cpu", "Shared resources", "One VM serves all workloads. The displayed CPU and memory values are configured limits, not live usage measurements.")
                maturityLine("leaf", "Quiet while idle", "The app refreshes on launch, activation, manual refresh, and action completion. It does not poll in the background.")
                maturityLine("wrench.and.screwdriver", "Current limits", "Rosetta, Mac home sharing, and interactive machine terminals are unavailable. Machine disks enforce capacity limits. Runtime, distribution, and Kubernetes support are still being validated.")
                maturityLine("chart.bar", "Performance is unverified", "Matching or reducing OrbStack's resource use is a project goal. No benchmark or feature parity claim is made by this app.")
            }
            HStack {
                Text("0.1.0 / Apple Silicon / macOS 15+").font(.system(size: 10)).foregroundStyle(.tertiary)
                Spacer()
                Button("Done") { dismiss() }.keyboardShortcut(.defaultAction)
            }
        }
        .padding(28)
        .frame(width: 550)
        .tint(SapphireTheme.blue)
    }

    private func maturityLine(_ symbol: String, _ title: String, _ message: String) -> some View {
        HStack(alignment: .top, spacing: 12) {
            Image(systemName: symbol).font(.system(size: 15)).foregroundStyle(SapphireTheme.blue)
                .frame(width: 21).padding(.top, 2)
            VStack(alignment: .leading, spacing: 4) {
                Text(title).font(.system(size: 12, weight: .semibold))
                Text(message).font(.system(size: 11)).foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
    }
}

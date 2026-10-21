import SwiftUI

enum SapphireTheme {
    static let blue = Color(red: 0.16, green: 0.39, blue: 0.94)
    static let ink = Color(nsColor: .labelColor)
    static let secondary = Color(nsColor: .secondaryLabelColor)
    static let surface = Color(nsColor: .controlBackgroundColor)
    static let canvas = Color(nsColor: .windowBackgroundColor)
    static let line = Color.primary.opacity(0.075)
    static let green = Color(red: 0.17, green: 0.61, blue: 0.42)
}

struct SapphireMark: View {
    var size: CGFloat = 36

    var body: some View {
        ZStack {
            RoundedRectangle(cornerRadius: size * 0.27, style: .continuous)
                .fill(LinearGradient(
                    colors: [Color(red: 0.35, green: 0.67, blue: 1), SapphireTheme.blue, Color(red: 0.12, green: 0.23, blue: 0.68)],
                    startPoint: .topLeading, endPoint: .bottomTrailing
                ))
            SapphireGem()
                .fill(.white.opacity(0.95))
                .frame(width: size * 0.58, height: size * 0.61)
            SapphireFacet()
                .stroke(SapphireTheme.blue.opacity(0.55), lineWidth: max(1, size * 0.026))
                .frame(width: size * 0.58, height: size * 0.61)
        }
        .frame(width: size, height: size)
        .shadow(color: SapphireTheme.blue.opacity(0.18), radius: 8, y: 3)
        .accessibilityLabel("Sapphire")
    }
}

private struct SapphireGem: Shape {
    func path(in rect: CGRect) -> Path {
        var path = Path()
        path.move(to: CGPoint(x: rect.midX, y: rect.minY))
        path.addLines([
            CGPoint(x: rect.maxX, y: rect.height * 0.25),
            CGPoint(x: rect.width * 0.9, y: rect.height * 0.68),
            CGPoint(x: rect.midX, y: rect.maxY),
            CGPoint(x: rect.width * 0.1, y: rect.height * 0.68),
            CGPoint(x: rect.minX, y: rect.height * 0.25),
        ])
        path.closeSubpath()
        return path
    }
}

private struct SapphireFacet: Shape {
    func path(in rect: CGRect) -> Path {
        var path = Path()
        path.move(to: CGPoint(x: rect.minX, y: rect.height * 0.25))
        path.addLine(to: CGPoint(x: rect.midX, y: rect.height * 0.42))
        path.addLine(to: CGPoint(x: rect.maxX, y: rect.height * 0.25))
        path.move(to: CGPoint(x: rect.midX, y: rect.minY))
        path.addLine(to: CGPoint(x: rect.midX, y: rect.maxY))
        path.move(to: CGPoint(x: rect.width * 0.1, y: rect.height * 0.68))
        path.addLine(to: CGPoint(x: rect.midX, y: rect.height * 0.42))
        path.addLine(to: CGPoint(x: rect.width * 0.9, y: rect.height * 0.68))
        return path
    }
}

struct StatusBadge: View {
    let title: String
    var active = false
    var neutral = false

    private var color: Color {
        neutral ? SapphireTheme.secondary : (active ? SapphireTheme.green : SapphireTheme.secondary)
    }

    var body: some View {
        HStack(spacing: 5) {
            Circle().fill(color).frame(width: 5, height: 5)
            Text(title).font(.system(size: 10, weight: .medium))
                .lineLimit(1)
        }
        .foregroundStyle(color)
        .padding(.horizontal, 8)
        .padding(.vertical, 4)
        .background(color.opacity(0.09), in: Capsule())
        .accessibilityLabel(title)
    }
}

struct ResourceIcon: View {
    let symbol: String
    var active = false
    var size: CGFloat = 36

    var body: some View {
        Image(systemName: symbol)
            .font(.system(size: size * 0.43, weight: .medium))
            .foregroundStyle(active ? SapphireTheme.blue : SapphireTheme.secondary)
            .frame(width: size, height: size)
            .background(
                active ? SapphireTheme.blue.opacity(0.09) : Color.primary.opacity(0.045),
                in: RoundedRectangle(cornerRadius: size * 0.27)
            )
    }
}

struct QuietButtonStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.system(size: 12, weight: .medium))
            .padding(.horizontal, 11)
            .padding(.vertical, 7)
            .background(Color.primary.opacity(configuration.isPressed ? 0.09 : 0.045), in: RoundedRectangle(cornerRadius: 7))
            .contentShape(RoundedRectangle(cornerRadius: 7))
    }
}

struct CodePanel: View {
    let text: String
    var minHeight: CGFloat = 150

    var body: some View {
        ScrollView([.horizontal, .vertical]) {
            Text(text.isEmpty ? "No output returned." : text)
                .font(.system(size: 11, design: .monospaced))
                .foregroundStyle(SapphireTheme.ink.opacity(0.88))
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .topLeading)
                .padding(16)
        }
        .frame(minHeight: minHeight, maxHeight: .infinity)
        .background(Color.primary.opacity(0.025), in: RoundedRectangle(cornerRadius: 9))
        .overlay(RoundedRectangle(cornerRadius: 9).stroke(SapphireTheme.line))
    }
}

struct NoticeView: View {
    let title: String
    let message: String
    var symbol = "info.circle"
    var tint = SapphireTheme.blue

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: symbol).foregroundStyle(tint).padding(.top, 1)
            VStack(alignment: .leading, spacing: 4) {
                Text(title).font(.system(size: 12, weight: .semibold))
                Text(message).font(.system(size: 11)).foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                    .textSelection(.enabled)
            }
            Spacer(minLength: 0)
        }
        .padding(13)
        .background(tint.opacity(0.055), in: RoundedRectangle(cornerRadius: 9))
        .overlay(RoundedRectangle(cornerRadius: 9).stroke(tint.opacity(0.12)))
    }
}

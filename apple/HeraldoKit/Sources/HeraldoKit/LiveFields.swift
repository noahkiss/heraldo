import Foundation

/// The Live Activity fields a Shortcut passes, in Notify!'s shape: progress
/// in percent, a countdown in seconds from now, steps and step, metrics as
/// text, and one button. `patch` turns them into envelope content.
public struct LiveFields: Hashable, Sendable {
    public var title: String?
    public var body: String?
    public var symbol: String?
    /// Percent done, 0 to 100.
    public var progress: Double?
    /// Seconds from now until the work ends.
    public var endsIn: Int?
    public var steps: Int?
    public var step: Int?
    public var tint: String?
    public var trailing: String?
    public var status: String?
    /// "Label: value, Label: value"; see LiveText.metrics.
    public var metricsText: String?
    public var buttonTitle: String?
    public var buttonURL: String?
    /// true: the button opens the URL. false: the device calls it.
    public var buttonOpensLink = true

    public init() {}

    /// Fields an update can clear, as Notify! names them.
    public enum Clear: String, CaseIterable, Sendable {
        case countdown, stages, metrics, button, trailing, status
    }

    /// The content for these fields at `now`. A field left nil is kept; a
    /// field in `clear` is cleared, unless the same call sets it.
    public func patch(clearing clear: Set<Clear> = [], now: Date = .now) -> LivePatch {
        var p = LivePatch()
        func text(_ s: String?, _ limit: Int) -> Patch<String> {
            guard let s = s?.trimmingCharacters(in: .whitespacesAndNewlines), !s.isEmpty else { return .keep }
            return .set(String(s.prefix(limit)))
        }
        p.title = text(title, 120)
        p.body = text(body, 300)
        p.symbol = text(symbol, 128)
        if let t = tint?.trimmingCharacters(in: .whitespaces), LiveTint(t) != nil { p.tint = .set(t) }
        p.trailing = text(trailing, 40)
        p.status = text(status, 40)
        if let v = progress { p.progress = .set(min(max(v, 0), 100) / 100) }
        if let total = steps, total >= 1 {
            p.steps = .set(LiveSteps(total: total, current: min(max(step ?? 0, 0), total)))
        }
        if let s = endsIn, s > 0 { p.endsAt = .set(now.addingTimeInterval(TimeInterval(s))) }
        if let m = metricsText.map(LiveText.metrics), !m.isEmpty { p.metrics = .set(m) }
        if let t = buttonTitle?.trimmingCharacters(in: .whitespaces), !t.isEmpty,
           let u = buttonURL?.trimmingCharacters(in: .whitespaces), URL(string: u)?.scheme != nil {
            p.button = .set(buttonOpensLink
                ? LiveButton(id: "button", title: String(t.prefix(64)), type: "open", url: u)
                : LiveButton(id: "button", title: String(t.prefix(64)), type: "http", url: u, method: "POST"))
        }
        for c in clear {
            switch c {
            case .countdown: if case .keep = p.endsAt { p.endsAt = .clear }
            case .stages:
                if case .keep = p.steps { p.steps = .clear }
            case .metrics: if case .keep = p.metrics { p.metrics = .clear }
            case .button: if case .keep = p.button { p.button = .clear }
            case .trailing: if case .keep = p.trailing { p.trailing = .clear }
            case .status: if case .keep = p.status { p.status = .clear }
            }
        }
        return p
    }
}

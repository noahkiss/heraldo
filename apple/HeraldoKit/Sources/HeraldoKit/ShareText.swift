import Foundation

/// Share out: one event as text another app can take.
extension Event {
    /// The link that opens this event in the apps.
    public var shareLink: URL { URL(string: "heraldo://event/\(id)")! }

    /// The event as markdown: a heading, the summary and body, a list of
    /// facts, and the heraldo:// link on the last line. The server side has
    /// the same text (`client.Markdown` in Go, `heraldo show --md`); change
    /// both together.
    public var shareMarkdown: String {
        func line(_ s: String) -> String {
            s.replacingOccurrences(of: "\r\n", with: " ")
                .replacingOccurrences(of: "\n", with: " ")
                .replacingOccurrences(of: "\r", with: " ")
                .trimmingCharacters(in: .whitespaces)
        }
        func trimmed(_ s: String?) -> String { (s ?? "").trimmingCharacters(in: .whitespacesAndNewlines) }

        var head = line(source)
        for s in [title, summary] where !trimmed(s).isEmpty {
            head = line(s!)
            break
        }
        var out = "# \(head)\n"
        let summary = trimmed(self.summary)
        if !summary.isEmpty, line(summary) != head { out += "\n\(summary)\n" }
        let body = trimmed(self.body)
        if !body.isEmpty { out += "\n\(body)\n" }
        out += "\n"
        func fact(_ k: String, _ v: String?) {
            let v = line(v ?? "")
            if !v.isEmpty { out += "- \(k): \(v)\n" }
        }
        fact("Source", source)
        fact("Host", host)
        let kind = self.kind ?? "message"
        if kind != "message" || key != nil {
            fact("Kind", [kind, action].compactMap { $0 }.joined(separator: " "))
        }
        fact("Key", key)
        fact("Level", level ?? "active")
        fact("Project", project)
        if let d = date {
            let f = ISO8601DateFormatter()
            f.timeZone = TimeZone(identifier: "UTC")
            fact("Time", f.string(from: Date(timeIntervalSince1970: d.timeIntervalSince1970.rounded(.down))))
        }
        fact("Link", url)
        if let tags { fact("Tags", tags.joined(separator: ", ")) }
        if let info = attachmentInfo, !info.isEmpty {
            fact("Attachments", "\(info.count) (\(info.map(\.contentType).joined(separator: ", ")))")
        }
        out += "\n\(shareLink.absoluteString)\n"
        return out
    }
}

/// Parses the event id out of a heraldo://event/<id> link, or returns the
/// text itself when it is a bare id.
public func eventID(fromLinkOrID raw: String) -> String? {
    let s = raw.trimmingCharacters(in: .whitespacesAndNewlines)
    if let url = URL(string: s), url.scheme == "heraldo", url.host() == "event" {
        return url.pathComponents.dropFirst().first
    }
    guard !s.isEmpty, !s.contains("/"), !s.contains(" ") else { return nil }
    return s
}

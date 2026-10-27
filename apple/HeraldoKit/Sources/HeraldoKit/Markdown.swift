import Foundation

/// A block of an event body. SwiftUI's Text renders inline markdown only, so
/// the detail view lays out these blocks and renders each one's inline text.
public enum MarkdownBlock: Equatable, Sendable {
    case heading(level: Int, text: String)
    case paragraph(String)
    case bullet(String)
    case numbered(Int, String)
    case quote(String)
    case code(String)
    case rule
}

public enum Markdown {
    /// Splits a body into blocks: ATX headings, fenced code, bullet and
    /// numbered list items, quotes, rules, and paragraphs.
    public static func blocks(_ text: String) -> [MarkdownBlock] {
        var out: [MarkdownBlock] = []
        var paragraph: [String] = []
        var code: [String]?

        func flush() {
            if !paragraph.isEmpty {
                out.append(.paragraph(paragraph.joined(separator: "\n")))
                paragraph = []
            }
        }

        for raw in text.replacingOccurrences(of: "\r\n", with: "\n").components(separatedBy: "\n") {
            let line = raw.trimmingCharacters(in: .whitespaces)
            if var lines = code {
                if line.hasPrefix("```") {
                    out.append(.code(lines.joined(separator: "\n")))
                    code = nil
                } else {
                    lines.append(raw)
                    code = lines
                }
                continue
            }
            if line.hasPrefix("```") {
                flush()
                code = []
            } else if line.isEmpty {
                flush()
            } else if let h = heading(line) {
                flush()
                out.append(h)
            } else if line == "---" || line == "***" || line == "___" {
                flush()
                out.append(.rule)
            } else if let item = prefix(line, ["- ", "* ", "+ "]) {
                flush()
                out.append(.bullet(item))
            } else if let (n, item) = numbered(line) {
                flush()
                out.append(.numbered(n, item))
            } else if line.hasPrefix(">") {
                flush()
                out.append(.quote(line.dropFirst().trimmingCharacters(in: .whitespaces)))
            } else {
                paragraph.append(line)
            }
        }
        if let code { out.append(.code(code.joined(separator: "\n"))) }
        flush()
        return out
    }

    /// The body as plain text, for a notification: markup gone, one line per block.
    public static func plainText(_ text: String) -> String {
        blocks(text).map { block -> String in
            switch block {
            case .heading(_, let t), .paragraph(let t), .quote(let t): return inline(t)
            case .bullet(let t): return "• " + inline(t)
            case .numbered(let n, let t): return "\(n). " + inline(t)
            case .code(let t): return t
            case .rule: return "—"
            }
        }.joined(separator: "\n")
    }

    /// Inline markdown with the markup removed.
    public static func inline(_ text: String) -> String {
        let options = AttributedString.MarkdownParsingOptions(interpretedSyntax: .inlineOnlyPreservingWhitespace)
        guard let a = try? AttributedString(markdown: text, options: options) else { return text }
        return String(a.characters)
    }

    private static func heading(_ line: String) -> MarkdownBlock? {
        let hashes = line.prefix { $0 == "#" }.count
        guard (1...6).contains(hashes), line.dropFirst(hashes).first == " " else { return nil }
        return .heading(level: hashes, text: line.dropFirst(hashes + 1).trimmingCharacters(in: .whitespaces))
    }

    private static func prefix(_ line: String, _ markers: [String]) -> String? {
        for m in markers where line.hasPrefix(m) {
            return String(line.dropFirst(m.count))
        }
        return nil
    }

    private static func numbered(_ line: String) -> (Int, String)? {
        let digits = line.prefix { $0.isNumber }
        guard !digits.isEmpty, digits.count <= 9, let n = Int(digits) else { return nil }
        let rest = line.dropFirst(digits.count)
        guard rest.hasPrefix(". ") || rest.hasPrefix(") ") else { return nil }
        return (n, String(rest.dropFirst(2)))
    }
}

import Foundation

/// A guidance line: free text that informs the server's LLM about wording
/// and grouping. From `GET /v1/guidance`.
public struct Guidance: Codable, Hashable, Identifiable, Sendable {
    public var id: String
    /// global, project:<p>, source:<s> or key:<regex>.
    public var scope: String
    public var text: String
    public var enabled: Bool
    public var version: Int
    public var createdBy: String?
    public var updatedAt: Date?

    enum CodingKeys: String, CodingKey {
        case id, scope, text, enabled, version
        case createdBy = "created_by"
        case updatedAt = "updated_at"
    }

    public init(id: String, scope: String, text: String, enabled: Bool = true, version: Int = 1,
                createdBy: String? = nil, updatedAt: Date? = nil) {
        self.id = id
        self.scope = scope
        self.text = text
        self.enabled = enabled
        self.version = version
        self.createdBy = createdBy
        self.updatedAt = updatedAt
    }
}

/// The body of `POST /v1/guidance`, and a proposal's guidance.
public struct GuidanceInput: Codable, Hashable, Sendable {
    public var scope: String
    public var text: String
    public var enabled: Bool?

    public init(scope: String, text: String, enabled: Bool? = nil) {
        self.scope = scope
        self.text = text
        self.enabled = enabled
    }
}

/// The body of `PUT /v1/guidance/{id}`: the fields given replace the stored ones.
public struct GuidancePatch: Codable, Hashable, Sendable {
    public var scope: String?
    public var text: String?
    public var enabled: Bool?

    public init(scope: String? = nil, text: String? = nil, enabled: Bool? = nil) {
        self.scope = scope
        self.text = text
        self.enabled = enabled
    }
}

struct GuidanceList: Codable, Sendable {
    var guidance: [Guidance]
}

/// `GET /v1/llm/status`: the server's LLM assistant.
public struct LLMStatus: Codable, Hashable, Sendable {
    public var mode: String
    public var effectiveMode: String
    /// Questions are allowed now.
    public var chat: Bool
    public var configured: Bool
    public var circuit: String?
    public var lastError: String?

    enum CodingKeys: String, CodingKey {
        case mode, chat, configured, circuit
        case effectiveMode = "effective_mode"
        case lastError = "last_error"
    }

    public init(mode: String, effectiveMode: String, chat: Bool, configured: Bool, circuit: String? = nil,
                lastError: String? = nil) {
        self.mode = mode
        self.effectiveMode = effectiveMode
        self.chat = chat
        self.configured = configured
        self.circuit = circuit
        self.lastError = lastError
    }
}

/// What a question is about.
public struct AskScope: Codable, Hashable, Sendable {
    /// A saved view's id or name.
    public var view: String?
    public var project: String?
    /// One incident's or job's key.
    public var key: String?
    /// RFC 3339, or a duration back from now such as 12h.
    public var since: String?

    public init(view: String? = nil, project: String? = nil, key: String? = nil, since: String? = nil) {
        self.view = view
        self.project = project
        self.key = key
        self.since = since
    }

    public var isEmpty: Bool { self == AskScope() }
}

/// The body of `POST /v1/ask`.
public struct AskInput: Codable, Hashable, Sendable {
    public var question: String
    public var scope: AskScope?

    public init(question: String, scope: AskScope? = nil) {
        self.question = question
        self.scope = scope.flatMap { $0.isEmpty ? nil : $0 }
    }
}

/// A change the assistant suggests. The server applied nothing; a client
/// applies it with the ordinary write call when the person agrees.
public struct AskProposal: Codable, Hashable, Sendable {
    /// filter, view, guidance or mute.
    public var type: String
    public var summary: String?
    public var filter: ViewFilter?
    public var view: ViewInput?
    public var guidance: GuidanceInput?
    public var mute: MuteInput?

    public init(type: String, summary: String? = nil, filter: ViewFilter? = nil, view: ViewInput? = nil,
                guidance: GuidanceInput? = nil, mute: MuteInput? = nil) {
        self.type = type
        self.summary = summary
        self.filter = filter
        self.view = view
        self.guidance = guidance
        self.mute = mute
    }
}

/// `POST /v1/ask`'s answer.
public struct AskAnswer: Codable, Hashable, Sendable {
    public var answer: String
    /// Events the answer rests on.
    public var citedIDs: [String]
    public var proposal: AskProposal?
    public var model: String?

    enum CodingKeys: String, CodingKey {
        case answer, proposal, model
        case citedIDs = "cited_ids"
    }

    public init(answer: String, citedIDs: [String] = [], proposal: AskProposal? = nil, model: String? = nil) {
        self.answer = answer
        self.citedIDs = citedIDs
        self.proposal = proposal
        self.model = model
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        answer = try c.decode(String.self, forKey: .answer)
        citedIDs = try c.decodeIfPresent([String].self, forKey: .citedIDs) ?? []
        // A proposal this build cannot read is left out, not an error.
        proposal = try? c.decodeIfPresent(AskProposal.self, forKey: .proposal)
        model = try c.decodeIfPresent(String.self, forKey: .model)
    }
}

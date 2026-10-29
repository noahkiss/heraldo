import XCTest
@testable import HeraldoKit

/// The assistant: ask, guidance and the LLM status, decoded from
/// hand-written fixtures.
final class AssistantTests: XCTestCase {
    func testDecodesAnswerWithProposal() throws {
        let json = """
        {"answer": "The NAS disk is 91% full.", "cited_ids": ["01JAAAAAAAAAAAAAAAAAAAAAAA"],
         "proposal": {"type": "mute", "summary": "Mute the disk alert for an hour", "mute": {"key": "disk:nas", "for": "1h"}},
         "call_ids": ["llm_1"], "model": "m"}
        """
        let a = try JSON.decoder.decode(AskAnswer.self, from: Data(json.utf8))
        XCTAssertEqual(a.citedIDs, ["01JAAAAAAAAAAAAAAAAAAAAAAA"])
        XCTAssertEqual(a.proposal?.type, "mute")
        XCTAssertEqual(a.proposal?.mute?.key, "disk:nas")
        XCTAssertEqual(a.proposal?.mute?.for, "1h")
    }

    func testUnreadableProposalIsLeftOut() throws {
        let json = #"{"answer": "ok", "cited_ids": [], "proposal": {"summary": "no type"}}"#
        let a = try JSON.decoder.decode(AskAnswer.self, from: Data(json.utf8))
        XCTAssertEqual(a.answer, "ok")
        XCTAssertNil(a.proposal)
    }

    func testAskInputLeavesOutAnEmptyScope() throws {
        let plain = String(decoding: try JSON.encoder.encode(AskInput(question: "hi", scope: AskScope())), as: UTF8.self)
        XCTAssertEqual(plain, #"{"question":"hi"}"#)
        let scoped = String(decoding: try JSON.encoder.encode(AskInput(question: "hi", scope: AskScope(key: "disk:nas"))),
                            as: UTF8.self)
        XCTAssertEqual(scoped, #"{"question":"hi","scope":{"key":"disk:nas"}}"#)
    }

    func testDecodesGuidanceAndStatus() throws {
        let g = try JSON.decoder.decode(GuidanceList.self, from: Data("""
        {"guidance": [{"id": "gd_1", "scope": "project:home", "text": "Mention the host.", "enabled": false,
          "version": 2, "created_by": "phone", "created_at": "2026-10-29T00:00:00Z", "updated_at": "2026-10-29T00:00:00Z"}]}
        """.utf8))
        XCTAssertEqual(g.guidance.first?.scope, "project:home")
        XCTAssertEqual(g.guidance.first?.enabled, false)
        let s = try JSON.decoder.decode(LLMStatus.self, from: Data("""
        {"mode": "shadow", "effective_mode": "shadow", "chat": true, "configured": true, "token_set": false,
         "circuit": "closed", "tokens_today": 0, "tokens_per_day": 200000, "calls_last_minute": 0, "calls_per_minute": 10}
        """.utf8))
        XCTAssertTrue(s.chat)
        XCTAssertEqual(s.effectiveMode, "shadow")
    }
}

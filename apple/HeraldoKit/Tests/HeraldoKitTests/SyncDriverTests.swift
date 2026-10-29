import XCTest
@testable import HeraldoKit

/// A clock the test moves by hand.
@MainActor
final class ManualClock: SyncClock {
    var now = Date(timeIntervalSince1970: 1_000_000)
    private var timers: [Timer] = []

    final class Timer: SyncTimer {
        let at: Date
        let body: @MainActor () -> Void
        var cancelled = false
        init(at: Date, body: @escaping @MainActor () -> Void) {
            self.at = at
            self.body = body
        }
        func cancel() { cancelled = true }
    }

    func after(_ seconds: TimeInterval, _ body: @escaping @MainActor () -> Void) -> SyncTimer {
        let t = Timer(at: now.addingTimeInterval(seconds), body: body)
        timers.append(t)
        return t
    }

    /// Moves the clock and fires the timers that came due, in order.
    func advance(_ seconds: TimeInterval) {
        let end = now.addingTimeInterval(seconds)
        while let next = timers.filter({ !$0.cancelled && $0.at <= end }).min(by: { $0.at < $1.at }) {
            now = next.at
            timers.removeAll { $0 === next }
            next.body()
        }
        now = end
    }

    var pending: Int { timers.filter { !$0.cancelled }.count }
}

@MainActor
final class FakeStream: SyncStreamControl {
    var heartbeat: Int?
    var starts = 0
    func start(heartbeat: Int) {
        self.heartbeat = heartbeat
        starts += 1
    }
    func stop() { heartbeat = nil }
}

@MainActor
final class FakeSafetyNet: SafetyNetControl {
    var net: SafetyNet?
    func schedule(_ net: SafetyNet) { self.net = net }
    func cancel() { net = nil }
}

/// The driver's state machine: inputs to stream and safety net, and
/// triggers to debounced requests.
@MainActor
final class SyncDriverTests: XCTestCase {
    var clock: ManualClock!
    var stream: FakeStream!
    var net: FakeSafetyNet!
    var requests: [SyncRequest] = []

    override func setUp() async throws {
        clock = ManualClock()
        stream = FakeStream()
        net = FakeSafetyNet()
        requests = []
    }

    private func driver(_ inputs: SyncInputs = SyncInputs(configured: true, network: .satisfied),
                        config: SyncDriver.Config = .init()) -> SyncDriver {
        SyncDriver(inputs: inputs, config: config, clock: clock, stream: stream, safetyNet: net) { [weak self] r in
            self?.requests.append(r)
        }
    }

    /// Lets the tasks the driver started run.
    private func settle() async {
        for _ in 0..<5 { await Task.yield() }
    }

    // MARK: Mode table

    func testModeTable() {
        let on = SyncInputs(configured: true, network: .satisfied)
        var i = on
        i.windowVisible = true
        XCTAssertEqual(SyncMode.of(i), SyncMode(heartbeat: 25, safetyNet: .normal, uiLive: true))
        XCTAssertEqual(SyncMode.of(on), SyncMode(heartbeat: 60, safetyNet: .normal, uiLive: false))
        var low = on
        low.lowPower = true
        XCTAssertEqual(SyncMode.of(low), SyncMode(heartbeat: nil, safetyNet: .saving, uiLive: false))
        low.windowVisible = true
        XCTAssertEqual(SyncMode.of(low), SyncMode(heartbeat: 25, safetyNet: .saving, uiLive: true))
        var hotspot = on
        hotspot.expensive = true
        XCTAssertEqual(SyncMode.of(hotspot).heartbeat, nil)
        var lowData = on
        lowData.constrained = true
        XCTAssertEqual(SyncMode.of(lowData).safetyNet, .saving)
        var asleep = on
        asleep.awake = false
        asleep.windowVisible = true
        XCTAssertEqual(SyncMode.of(asleep), SyncMode(heartbeat: nil, safetyNet: nil, uiLive: false))
        var offline = on
        offline.network = .unsatisfied
        XCTAssertEqual(SyncMode.of(offline), SyncMode(heartbeat: nil, safetyNet: nil, uiLive: false))
        XCTAssertEqual(SyncMode.of(SyncInputs()), SyncMode(heartbeat: nil, safetyNet: nil, uiLive: false))
        // An unknown path counts as up.
        XCTAssertEqual(SyncMode.of(SyncInputs(configured: true)).heartbeat, 60)
    }

    // MARK: Stream and safety net

    func testMenuBarOnlyOpensStreamAndNet() {
        let d = driver()
        XCTAssertEqual(stream.heartbeat, 60)
        XCTAssertEqual(net.net, .normal)
        XCTAssertEqual(d.streamHeartbeat, 60)
    }

    func testLowPowerClosesStreamAndSlowsNet() {
        let d = driver()
        d.update { $0.lowPower = true }
        XCTAssertNil(stream.heartbeat)
        XCTAssertEqual(net.net, .saving)
        // A window opens: the stream comes back for it.
        d.update { $0.windowVisible = true }
        XCTAssertEqual(stream.heartbeat, 25)
        d.update { $0.windowVisible = false }
        XCTAssertNil(stream.heartbeat)
        d.update { $0.lowPower = false }
        XCTAssertEqual(stream.heartbeat, 60)
        XCTAssertEqual(net.net, .normal)
    }

    func testHeartbeatChangeAloneDoesNotReconnect() {
        let d = driver()
        XCTAssertEqual(stream.starts, 1)
        d.update { $0.windowVisible = true }
        XCTAssertEqual(stream.starts, 1)
        XCTAssertEqual(d.mode.heartbeat, 25)
        XCTAssertEqual(d.streamHeartbeat, 60)
    }

    func testSleepClosesEverythingAndWakeReopens() {
        let d = driver()
        d.update { $0.awake = false }
        XCTAssertNil(stream.heartbeat)
        XCTAssertNil(net.net)
        d.update { $0.awake = true }
        XCTAssertEqual(stream.heartbeat, 60)
        XCTAssertEqual(stream.starts, 2)
        XCTAssertEqual(net.net, .normal)
    }

    func testNoNetworkClosesWithoutRetrying() {
        let d = driver()
        d.update { $0.network = .unsatisfied }
        XCTAssertNil(stream.heartbeat)
        XCTAssertNil(net.net)
        XCTAssertEqual(clock.pending, 0)
        d.update { $0.network = .satisfied }
        XCTAssertEqual(stream.heartbeat, 60)
    }

    func testWakeWithoutStreamChecksOnce() async {
        let d = driver(SyncInputs(configured: true, lowPower: true, network: .satisfied))
        d.update { $0.awake = false }
        d.update { $0.awake = true }
        clock.advance(2)
        await settle()
        XCTAssertEqual(requests, [.revisionsCheck])
        // Network back, still no stream: one more check.
        d.update { $0.network = .unsatisfied }
        d.update { $0.network = .satisfied }
        clock.advance(2)
        await settle()
        XCTAssertEqual(requests, [.revisionsCheck, .revisionsCheck])
    }

    func testStreamDelayOpensOnlyAfterAWhile() {
        let d = driver(SyncInputs(configured: true, awake: false, network: .satisfied),
                       config: .init(streamDelay: 3))
        d.update {
            $0.awake = true
            $0.windowVisible = true
        }
        XCTAssertNil(stream.heartbeat)
        clock.advance(1)
        // Left again before the delay: nothing opens.
        d.update {
            $0.awake = false
            $0.windowVisible = false
        }
        clock.advance(5)
        XCTAssertNil(stream.heartbeat)
        d.update {
            $0.awake = true
            $0.windowVisible = true
        }
        clock.advance(3)
        XCTAssertEqual(stream.heartbeat, 25)
    }

    func testNotConfiguredRunsNothingThenFullRefresh() async {
        let d = driver(SyncInputs(network: .satisfied))
        XCTAssertNil(stream.heartbeat)
        XCTAssertNil(net.net)
        d.opened()
        d.update { $0.configured = true }
        XCTAssertEqual(stream.heartbeat, 60)
        clock.advance(2)
        await settle()
        XCTAssertEqual(requests, [.fullRefresh])
    }

    func testUILiveCallback() {
        var seen: [Bool] = []
        let d = driver()
        d.onUILive = { seen.append($0) }
        d.update { $0.windowVisible = true }
        d.update { $0.lowPower = true }
        d.update { $0.windowVisible = false }
        XCTAssertEqual(seen, [true, false])
    }

    // MARK: Requests

    func testBurstOfPokesIsOneRequest() async {
        let d = driver()
        for n in 1...20 {
            d.receive(.poke(Poke(cursor: "c\(n)", revisions: Revisions(boot: "b", rev: ["events": Int64(n)]))))
            clock.advance(0.05)
        }
        XCTAssertTrue(requests.isEmpty)
        clock.advance(2)
        await settle()
        XCTAssertEqual(requests, [SyncRequest(vector: Revisions(boot: "b", rev: ["events": 20]), poke: true)])
    }

    func testHintIsAnsweredByAStreamVector() async {
        let d = driver()
        d.hint()
        clock.advance(0.5)
        let v = Revisions(boot: "b", rev: ["events": 2])
        d.receive(.poke(Poke(revisions: v)))
        clock.advance(2)
        await settle()
        // No GET /v1/revisions: the poke's vector answered the push.
        XCTAssertEqual(requests, [SyncRequest(vector: v, poke: true)])
    }

    func testHintWithoutStreamChecks() async {
        let d = driver()
        d.hint()
        clock.advance(2)
        await settle()
        XCTAssertEqual(requests, [.revisionsCheck])
    }

    func testOldServerPokeAsksForLegacyRefresh() async {
        let d = driver()
        d.receive(.poke(Poke(cursor: "c1")))
        clock.advance(2)
        await settle()
        XCTAssertEqual(requests, [SyncRequest(poke: true)])
        XCTAssertNil(d.lastCheck)
    }

    func testOpenedChecksOnlyWhenStale() async {
        let d = driver()
        d.opened()
        clock.advance(2)
        await settle()
        XCTAssertEqual(requests.count, 1)
        clock.advance(10)
        d.opened()
        clock.advance(2)
        await settle()
        XCTAssertEqual(requests.count, 1, "checked 12 s ago")
        clock.advance(30)
        d.opened()
        clock.advance(2)
        await settle()
        XCTAssertEqual(requests.count, 2)
        // A vector from the stream counts as a check.
        d.receive(.revisions(Revisions(boot: "b", rev: [:])))
        clock.advance(2)
        await settle()
        d.opened()
        clock.advance(2)
        await settle()
        XCTAssertEqual(requests.count, 3)
        XCTAssertEqual(requests.last?.vector, Revisions(boot: "b", rev: [:]))
    }

    func testSafetyNetAndSyncNowSkipTheDebounce() async {
        let d = driver()
        await d.safetyNetFired()
        XCTAssertEqual(requests, [.revisionsCheck])
        await d.syncNow()
        XCTAssertEqual(requests, [.revisionsCheck, .fullRefresh])
        d.update { $0.awake = false }
        await d.safetyNetFired()
        XCTAssertEqual(requests.count, 2, "asleep: the net does nothing")
    }

    func testKeepAliveDoesNothing() async {
        let d = driver()
        d.receive(.keepAlive)
        clock.advance(5)
        await settle()
        XCTAssertTrue(requests.isEmpty)
    }
}

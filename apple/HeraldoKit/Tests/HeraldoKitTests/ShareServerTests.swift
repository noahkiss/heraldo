import CoreGraphics
import ImageIO
import UniformTypeIdentifiers
import XCTest
@testable import HeraldoKit

/// Share in against a real heraldo-server (apple/scripts/test-server.sh);
/// without one these skip. The image shrink test needs no server.
final class ShareServerTests: XCTestCase {
    func testSendUploadsAndPosts() async throws {
        let env = ProcessInfo.processInfo.environment
        guard let url = env["HERALDO_TEST_URL"].flatMap(URL.init(string:)),
              let read = env["HERALDO_TEST_READ_TOKEN"], let write = env["HERALDO_TEST_WRITE_TOKEN"] else {
            throw XCTSkip("no test server; run apple/scripts/test-server.sh")
        }
        let client = HeraldoClient(baseURL: url, readToken: read, writeToken: write)
        let source = "share:test-\(UUID().uuidString.prefix(8).lowercased())"
        let file = ShareFile(name: "note.txt", data: Data("hello \(source)".utf8), contentType: "text/plain")
        let draft = ShareDraft(title: "Shared note", text: "from the test", files: [file], project: "sharetest")
        let e = try await ShareSender.send(draft, with: client, source: source)
        XCTAssertEqual(e.level, "passive")
        XCTAssertEqual(e.project, "sharetest")
        XCTAssertEqual(e.attachmentInfo?.first?.contentType, "text/plain")
        // The same draft again is the same event: dedup.
        let again = try await ShareSender.send(draft, with: client, source: source)
        XCTAssertEqual(again.id, e.id)
        let bytes = try await client.attachment(id: try XCTUnwrap(e.attachments?.first)).data
        XCTAssertEqual(bytes, file.data)
    }

    func testLargeImageShrinksToFit() throws {
        // Noise does not compress: a 2600 px square PNG is well over 5 MiB.
        let side = 2600
        var pixels = [UInt8](repeating: 0, count: side * side * 4)
        var x: UInt32 = 2463534242
        for i in pixels.indices {
            x ^= x << 13; x ^= x >> 17; x ^= x << 5
            pixels[i] = i % 4 == 3 ? 255 : UInt8(truncatingIfNeeded: x)
        }
        let ctx = try XCTUnwrap(CGContext(data: &pixels, width: side, height: side, bitsPerComponent: 8,
                                          bytesPerRow: side * 4, space: CGColorSpaceCreateDeviceRGB(),
                                          bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue))
        let img = try XCTUnwrap(ctx.makeImage())
        let png = NSMutableData()
        let dst = try XCTUnwrap(CGImageDestinationCreateWithData(png, UTType.png.identifier as CFString, 1, nil))
        CGImageDestinationAddImage(dst, img, nil)
        XCTAssertTrue(CGImageDestinationFinalize(dst))
        XCTAssertGreaterThan(png.length, ShareLimits.maxBytes)
        let f = try ShareFile.make(name: "noise.png", data: png as Data, type: .png)
        XCTAssertEqual(f.contentType, "image/jpeg")
        XCTAssertLessThanOrEqual(f.data.count, ShareLimits.maxBytes)
    }
}

import AppKit
import SwiftUI

/// The macOS share extension's principal class: hosts ShareView.
@objc(ShareViewController)
final class ShareViewController: NSViewController {
    private var model: ShareModel!

    override var nibName: NSNib.Name? { nil }

    override func loadView() {
        model = ShareModel { [weak self] sent in
            if sent {
                self?.extensionContext?.completeRequest(returningItems: nil)
            } else {
                self?.extensionContext?.cancelRequest(withError: CocoaError(.userCancelled))
            }
        }
        let host = NSHostingView(rootView: ShareView(model: model))
        host.frame = NSRect(x: 0, y: 0, width: 440, height: 520)
        view = host
        preferredContentSize = host.frame.size
    }

    override func viewDidLoad() {
        super.viewDidLoad()
        let items = extensionContext?.inputItems.compactMap { $0 as? NSExtensionItem } ?? []
        Task { await model.load(items) }
    }
}

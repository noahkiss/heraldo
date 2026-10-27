import HeraldoKit
import SwiftUI

@main
struct HeraldoApp: App {
    var body: some Scene {
        WindowGroup {
            ContentView()
        }
    }
}

struct ContentView: View {
    var body: some View {
        NavigationStack {
            ContentUnavailableView {
                Label {
                    Text("No notifications")
                } icon: {
                    Image("HeraldoMark")
                        .resizable()
                        .scaledToFit()
                        .frame(height: 64)
                        .accessibilityHidden(true)
                }
            } description: {
                Text("Events from the server appear here.")
            }
            .navigationTitle(AppInfo.displayName())
        }
        #if os(macOS)
        .frame(minWidth: 480, minHeight: 360)
        #endif
    }
}

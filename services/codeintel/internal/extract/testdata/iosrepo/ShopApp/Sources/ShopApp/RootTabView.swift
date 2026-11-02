import SwiftUI

struct RootTabView: View {
    var body: some View {
        TabView {
            HomeScreen().trooperView("Home")
                .tabItem { Label("Home", systemImage: "house") }
            OrdersView()
                .tabItem { Label("Orders", systemImage: "list.bullet") }
        }
    }
}

struct HomeScreen: View {
    var body: some View {
        NavigationStack {
            VStack {
                PromoBanner()
                NavigationLink("Profile", destination: ProfileView())
            }
        }
    }
}

/* A nested /* block */ comment:
struct CommentedScreen: View { var body: some View { Text("x") } }
*/
// struct AlsoCommentedScreen: View {}

let template = """
    struct TemplateScreen: View {
        var body: some View { NavigationLink(destination: TemplateTarget()) { Text("t") } }
    }
    """

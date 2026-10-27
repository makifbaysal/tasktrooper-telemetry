#if os(iOS)
import SwiftUI
import UIKit
import TaskTrooper
import TaskTrooperReplay

/// UIKit form for session replay: labels, text fields (one secure), a switch, an alert with a text field and a table.
final class FormViewController: UIViewController, UITableViewDataSource {
    private let table = UITableView(frame: .zero, style: .insetGrouped)
    var onClose: (() -> Void)?

    override func viewDidLoad() {
        super.viewDidLoad()
        title = "UIKit form"
        view.backgroundColor = .systemBackground

        let header = UILabel()
        header.text = "Sign in to TaskTrooper"
        header.font = .preferredFont(forTextStyle: .title2)
        header.trooperUnmask = true // static copy

        let email = UITextField()
        email.placeholder = "Email"
        email.borderStyle = .roundedRect
        email.accessibilityIdentifier = "email"
        email.autocapitalizationType = .none

        let password = UITextField()
        password.placeholder = "Password"
        password.borderStyle = .roundedRect
        password.isSecureTextEntry = true
        password.accessibilityIdentifier = "password"

        let remember = UISwitch()
        remember.accessibilityIdentifier = "remember"
        let rememberLabel = UILabel()
        rememberLabel.text = "Remember me"
        let row = UIStackView(arrangedSubviews: [rememberLabel, remember])
        row.spacing = 8

        var cfg = UIButton.Configuration.filled()
        cfg.title = "Show alert"
        cfg.baseBackgroundColor = .systemIndigo
        let alertButton = UIButton(configuration: cfg, primaryAction: UIAction { [weak self] _ in self?.showAlert() })
        alertButton.accessibilityIdentifier = "showAlert"

        let close = UIButton(type: .system, primaryAction: UIAction { [weak self] _ in self?.onClose?() })
        close.setTitle("Close", for: .normal)
        close.accessibilityIdentifier = "closeForm"

        let card = UIView()
        card.backgroundColor = .systemOrange
        card.layer.cornerRadius = 12
        card.heightAnchor.constraint(equalToConstant: 40).isActive = true

        let stack = UIStackView(arrangedSubviews: [header, email, password, row, alertButton, card, close])
        stack.axis = .vertical
        stack.spacing = 12

        table.dataSource = self
        table.register(UITableViewCell.self, forCellReuseIdentifier: "cell")
        table.accessibilityIdentifier = "table"

        for v in [stack, table] as [UIView] {
            v.translatesAutoresizingMaskIntoConstraints = false
            view.addSubview(v)
        }
        let g = view.safeAreaLayoutGuide
        NSLayoutConstraint.activate([
            stack.topAnchor.constraint(equalTo: g.topAnchor, constant: 16),
            stack.leadingAnchor.constraint(equalTo: g.leadingAnchor, constant: 20),
            stack.trailingAnchor.constraint(equalTo: g.trailingAnchor, constant: -20),
            table.topAnchor.constraint(equalTo: stack.bottomAnchor, constant: 12),
            table.leadingAnchor.constraint(equalTo: g.leadingAnchor),
            table.trailingAnchor.constraint(equalTo: g.trailingAnchor),
            table.bottomAnchor.constraint(equalTo: view.bottomAnchor),
        ])
    }

    override func viewDidAppear(_ animated: Bool) {
        super.viewDidAppear(animated)
        TaskTrooper.startView("UIKitForm")
    }

    private func showAlert() {
        let alert = UIAlertController(title: "Coupon", message: "Enter your coupon code", preferredStyle: .alert)
        alert.addTextField { $0.placeholder = "Code"; $0.accessibilityIdentifier = "coupon" }
        alert.addAction(UIAlertAction(title: "Cancel", style: .cancel))
        alert.addAction(UIAlertAction(title: "Apply", style: .default))
        present(alert, animated: true)
    }

    func tableView(_ tableView: UITableView, numberOfRowsInSection section: Int) -> Int { 40 }

    func tableView(_ tableView: UITableView, cellForRowAt indexPath: IndexPath) -> UITableViewCell {
        let cell = tableView.dequeueReusableCell(withIdentifier: "cell", for: indexPath)
        var c = cell.defaultContentConfiguration()
        c.text = "Order #\(1000 + indexPath.row)"
        c.secondaryText = "Customer \(indexPath.row)"
        c.image = UIImage(systemName: "shippingbox")
        cell.contentConfiguration = c
        return cell
    }
}

struct FormScreen: UIViewControllerRepresentable {
    var onClose: () -> Void
    func makeUIViewController(context: Context) -> UINavigationController {
        let vc = FormViewController()
        vc.onClose = onClose
        return UINavigationController(rootViewController: vc)
    }
    func updateUIViewController(_ vc: UINavigationController, context: Context) {}
}

/// SwiftUI screen for session replay: colored cards, a toggle, a text field and a long list.
struct SwiftUIListScreen: View {
    @State private var notify = true
    @State private var search = ""

    var body: some View {
        List {
            Section {
                HStack(spacing: 12) {
                    RoundedRectangle(cornerRadius: 10).fill(Color.blue).frame(width: 60, height: 40)
                    RoundedRectangle(cornerRadius: 10).fill(Color.green).frame(width: 60, height: 40)
                    Image(systemName: "star.fill").foregroundColor(.yellow)
                }
                Toggle("Notifications", isOn: $notify)
                TextField("Search orders", text: $search).accessibilityIdentifier("search")
            }
            Section("Orders") {
                ForEach(0..<40) { i in
                    HStack {
                        Image(systemName: "shippingbox")
                        Text("Order #\(2000 + i)")
                        Spacer()
                        Text("$\(i * 3).00").foregroundColor(.secondary)
                    }
                }
            }
        }
        .navigationTitle("SwiftUI list")
        .trooperView("SwiftUIList")
    }
}
#endif

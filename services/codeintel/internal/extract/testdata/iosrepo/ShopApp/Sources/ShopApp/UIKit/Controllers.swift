import UIKit
import TaskTrooper

class BaseViewController: UIViewController {
    override func viewDidLoad() { super.viewDidLoad() }
}

final class ProductViewController: BaseViewController, UITableViewDelegate {
    override func viewDidLoad() { super.viewDidLoad() }
}

extension ProductViewController: TrooperViewNaming {
    var trooperViewName: String? { "ProductDetail" }
}

class LegacyCheckoutViewController: UITableViewController {}

// Containers are not views (the SDK ignores them).
class MainNavigationController: UINavigationController {}

class PriceFormatter: NSObject {}

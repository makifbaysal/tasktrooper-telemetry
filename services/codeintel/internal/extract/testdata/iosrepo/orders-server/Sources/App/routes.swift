import Vapor

func routes(_ app: Application) throws {
    app.get("healthz") { req in "ok" }

    let api = app.grouped("api", "v1")
    api.get("orders", ":id", use: getOrder)
    api.on(.POST, "orders", body: .collect(maxSize: "1mb"), use: createOrder)

    app.group("admin") { admin in
        admin.delete("users", ":userID") { req -> HTTPStatus in .ok }
    }

    try api.register(collection: TodoController())
    // app.get("commented") { _ in "" }
    let doc = "app.get(\"in-string\")"
}

func getOrder(req: Request) async throws -> String {
    req.parameters.get("id") ?? ""
}

func createOrder(req: Request) async throws -> HTTPStatus { .created }

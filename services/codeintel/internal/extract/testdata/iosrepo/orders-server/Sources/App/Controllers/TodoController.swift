import Vapor
import FluentPostgresDriver

struct TodoController: RouteCollection {
    func boot(routes: RoutesBuilder) throws {
        let todos = routes.grouped("todos")
        todos.get(use: index)
        todos.post(use: create)
        todos.group(":todoID") { todo in
            todo.delete(use: delete)
        }
    }

    func index(req: Request) async throws -> [String] { [] }
    func create(req: Request) async throws -> String { "" }
    func delete(req: Request) async throws -> HTTPStatus { .noContent }
}

from flask import Flask, Blueprint
import redis

app = Flask(__name__)
bp = Blueprint("accounts", __name__, url_prefix="/accounts")


@app.route("/login", methods=["POST"])
def login():
    return "ok"


@bp.route("/<int:account_id>")
def get_account(account_id):
    return "x"


@bp.route("/<int:account_id>", methods=["DELETE"])
def delete_account(account_id):
    return "x"


app.register_blueprint(bp)

from fastapi import FastAPI
from app.routers import payments
import tasktrooper

tasktrooper.init(service="payments-py")
app = FastAPI()
app.include_router(payments.router, prefix="/v1")


@app.get("/healthz")
async def health():
    """Health check.

    @app.get("/in-docstring")
    """
    return {"ok": True}

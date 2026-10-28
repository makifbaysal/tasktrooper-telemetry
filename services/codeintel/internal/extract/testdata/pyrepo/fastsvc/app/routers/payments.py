from fastapi import APIRouter
import httpx
import stripe

router = APIRouter(prefix="/payments", tags=["payments"])


@router.post("/{payment_id}/capture")
async def capture_payment(payment_id: str):
    # @router.get("/commented")
    async with httpx.AsyncClient() as client:
        await client.post("https://api.iyzipay.com/payment/auth", json={})
    return {"id": payment_id}


@router.get(
    "/{payment_id}",
)
def get_payment(payment_id: str):
    return {}


@router.api_route("/webhook", methods=["POST", "PUT"])
def webhook():
    pass

from celery import shared_task
from shop.celery import app


@shared_task(name="orders.send_receipt", queue="emails")
def send_receipt(order_id):
    pass


@app.task
def charge_subscription(sub_id):
    pass

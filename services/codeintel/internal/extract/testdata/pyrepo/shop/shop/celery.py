from celery import Celery
from celery.schedules import crontab

app = Celery("shop")
app.conf.beat_schedule = {
    "nightly-settlement": {
        "task": "orders.tasks.charge_subscription",
        "schedule": crontab(minute=0, hour=2),
    },
    "every-30s-heartbeat": {
        "task": "orders.tasks.heartbeat",
        "schedule": 30.0,
    },
}

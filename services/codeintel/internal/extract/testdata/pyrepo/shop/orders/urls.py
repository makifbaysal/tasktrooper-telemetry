from django.urls import path
from . import views

urlpatterns = [
    path("", views.OrderList.as_view(), name="orders"),
    path("<int:pk>/refund/", views.refund_order, name="refund"),
]

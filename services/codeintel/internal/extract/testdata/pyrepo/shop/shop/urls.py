from django.urls import path, include, re_path
from . import views

urlpatterns = [
    path("admin/", admin.site.urls),
    path("api/orders/", include("orders.urls")),
    path("health/", views.health),
    re_path(r"^legacy/(?P<code>[A-Z]+)/$", views.legacy),
]

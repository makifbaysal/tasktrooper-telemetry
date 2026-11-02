import { Injectable, inject } from '@angular/core';
import { HttpClient } from '@angular/common/http';

@Injectable({ providedIn: 'root' })
export class OrdersService {
  private http = inject(HttpClient);

  list() {
    return this.http.get<Order[]>('https://api.admin.example.com/v1/orders');
  }

  refund(id: string) {
    return this.http.post('https://payments.admin.example.com/v1/refunds', { id });
  }
}

interface Order { id: string }

import { Routes } from '@angular/router';
import { DashboardComponent } from './dashboard.component';

export const routes: Routes = [
  { path: '', component: DashboardComponent },
  {
    path: 'orders',
    children: [
      { path: '', loadComponent: () => import('./orders/list.component').then((m) => m.OrderListComponent) },
      { path: ':orderId', loadComponent: () => import('./orders/detail.component').then((m) => m.OrderDetailComponent) },
      { path: ':orderId/refund', component: DashboardComponent, canActivate: [] },
    ],
  },
  { path: 'admin', loadChildren: () => import('./admin/admin.routes').then((m) => m.ADMIN_ROUTES) },
  { path: 'home', redirectTo: '', pathMatch: 'full' },
  { path: '**', component: DashboardComponent },
];

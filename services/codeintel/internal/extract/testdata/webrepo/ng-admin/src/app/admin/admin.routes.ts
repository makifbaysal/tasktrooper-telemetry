import { Routes } from '@angular/router';
import { UsersComponent } from './users.component';

export const ADMIN_ROUTES: Routes = [
  { path: '', component: UsersComponent },
  { path: 'users/:userId', component: UsersComponent },
];

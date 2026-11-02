import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import Home from '../views/Home.vue'

const accountRoutes: RouteRecordRaw[] = [
  { path: '', component: () => import('../views/Account.vue') },
  { path: 'addresses/:addressId(\\d+)', component: () => import('../views/Addresses.vue') },
]

const routes: RouteRecordRaw[] = [
  { path: '/', name: 'home', component: Home },
  {
    path: '/products',
    component: () => import('../views/ProductsLayout.vue'),
    children: [
      { path: '', component: () => import('../views/ProductList.vue') },
      { path: ':slug', component: () => import('../views/ProductDetail.vue') },
      { path: '/promo/:code', component: () => import('../views/Promo.vue') },
    ],
  },
  { path: '/account', children: accountRoutes },
  { path: '/old-cart', redirect: '/cart' },
  // { path: '/commented-route', component: Home },
  { path: '/:pathMatch(.*)*', component: () => import('../views/NotFound.vue') },
]

// not a route table: no component/children/redirect
const crumbs = [{ path: '/breadcrumb-only', label: 'Home' }]

export const router = createRouter({ history: createWebHistory(), routes })
export default router

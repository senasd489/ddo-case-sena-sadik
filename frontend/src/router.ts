import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import { useAuth, type Role } from './stores/auth'

const home: Record<Role, string> = {
  subscriber: '/portal', field_crew: '/crew', billing: '/subscribers', operator: '/outages', analyst: '/loss', admin: '/outages',
}

const r = (path: string, component: RouteRecordRaw['component'], roles?: Role[]): RouteRecordRaw =>
  ({ path, component, meta: { roles } }) as RouteRecordRaw

const staff: Role[] = ['field_crew', 'billing', 'operator', 'analyst', 'admin']

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    r('/login', () => import('./views/LoginView.vue')),
    { path: '/', redirect: () => (useAuth().role ? home[useAuth().role!] : '/login') },
    r('/portal', () => import('./views/subscriber/PortalView.vue'), ['subscriber']),
    r('/invoices/:id', () => import('./views/billing/InvoiceView.vue'), ['subscriber', 'billing', 'analyst', 'admin']),
    r('/crew', () => import('./views/crew/CrewView.vue'), ['field_crew']),
    r('/subscribers', () => import('./views/billing/SubscribersView.vue'), staff),
    r('/subscribers/:id', () => import('./views/billing/SubscriberView.vue'), staff),
    r('/invoices', () => import('./views/billing/InvoicesView.vue'), ['billing', 'analyst', 'admin']),
    r('/outages', () => import('./views/operator/OutagesView.vue'), ['operator', 'admin', 'field_crew', 'analyst']),
    r('/outages/:id', () => import('./views/operator/OutageView.vue'), ['operator', 'admin', 'field_crew', 'analyst']),
    r('/network', () => import('./views/operator/NetworkView.vue'), ['operator', 'admin', 'field_crew', 'analyst']),
    r('/crews', () => import('./views/operator/CrewsView.vue'), ['operator', 'admin']),
    r('/loss', () => import('./views/analyst/LossView.vue'), ['analyst', 'admin', 'operator']),
    r('/reliability', () => import('./views/analyst/ReliabilityView.vue'), ['analyst', 'admin', 'operator']),
    r('/admin/users', () => import('./views/admin/UsersView.vue'), ['admin']),
    r('/admin/audit', () => import('./views/admin/AuditView.vue'), ['admin']),
    { path: '/:pathMatch(.*)*', component: () => import('./views/NotFoundView.vue') },
  ],
})

// İstemci tarafı yönlendirme yalnızca kullanılabilirlik içindir; asıl yetki
// kontrolü sunucuda her uçta yapılır.
router.beforeEach(async (to) => {
  const auth = useAuth()
  if (!auth.ready) await auth.init()
  if (to.path === '/login') return auth.user ? '/' : true
  if (!auth.user) return { path: '/login', query: { next: to.fullPath } }
  const roles = to.meta.roles as Role[] | undefined
  if (roles && !roles.includes(auth.user.role)) return home[auth.user.role]
  return true
})

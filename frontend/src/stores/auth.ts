import { defineStore } from 'pinia'
import { api, hasTokens, setTokens, onSessionEnd } from '../api/client'

export type Role = 'subscriber' | 'field_crew' | 'billing' | 'operator' | 'analyst' | 'admin'
export type User = { user_id: number; email: string; role: Role; subscriber_id?: number; crew_id?: number }

export const roleLabel: Record<Role, string> = {
  subscriber: 'Abone',
  field_crew: 'Saha ekibi',
  billing: 'Faturalama uzmanı',
  operator: 'Şebeke operatörü',
  analyst: 'Analist',
  admin: 'Yönetici',
}

export const useAuth = defineStore('auth', {
  state: () => ({ user: null as User | null, ready: false }),
  getters: {
    role: (s) => s.user?.role,
    is: (s) => (...roles: Role[]) => !!s.user && roles.includes(s.user.role),
  },
  actions: {
    async init() {
      onSessionEnd(() => {
        this.user = null
        location.assign('/login')
      })
      if (hasTokens()) {
        try {
          this.user = await api<User>('/auth/me')
        } catch {
          setTokens(null)
        }
      }
      this.ready = true
    },
    async login(email: string, password: string) {
      const t = await api<{ access_token: string; refresh_token: string; user: User }>('/auth/login', {
        body: { email, password },
      })
      setTokens(t)
      this.user = t.user
    },
    async logout() {
      try {
        const raw = localStorage.getItem('enerjihat.tokens')
        const rt = raw ? JSON.parse(raw).refresh_token : ''
        if (rt) await api('/auth/logout', { body: { refresh_token: rt } })
      } catch {
        /* yoksay */
      }
      setTokens(null)
      this.user = null
    },
  },
})

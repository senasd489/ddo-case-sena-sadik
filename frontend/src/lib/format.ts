const tl = new Intl.NumberFormat('tr-TR', { style: 'currency', currency: 'TRY' })
const num = new Intl.NumberFormat('tr-TR', { maximumFractionDigits: 1 })
const dt = new Intl.DateTimeFormat('tr-TR', { dateStyle: 'short', timeStyle: 'short', timeZone: 'Europe/Istanbul' })
const d = new Intl.DateTimeFormat('tr-TR', { dateStyle: 'medium', timeZone: 'Europe/Istanbul' })

export const money = (kurus: number) => tl.format(kurus / 100)
export const kwh = (wh: number) => `${num.format(wh / 1000)} kWh`
export const n = (x: number) => num.format(x)
export const dateTime = (s?: string | null) => (s ? dt.format(new Date(s)) : '—')
export const date = (s?: string | null) => (s ? d.format(new Date(s)) : '—')
export const ago = (s?: string | null) => {
  if (!s) return '—'
  const sec = Math.round((Date.now() - new Date(s).getTime()) / 1000)
  if (sec < 60) return `${sec} sn önce`
  if (sec < 3600) return `${Math.round(sec / 60)} dk önce`
  return `${Math.round(sec / 3600)} sa önce`
}
export const todayISO = () => new Date(Date.now() + 3 * 3600e3).toISOString().slice(0, 10)
export const daysAgoISO = (k: number) => new Date(Date.now() + 3 * 3600e3 - k * 86400e3).toISOString().slice(0, 10)
export const periodISO = (offsetMonths = 0) => {
  const t = new Date(Date.now() + 3 * 3600e3)
  t.setUTCDate(1)
  t.setUTCMonth(t.getUTCMonth() + offsetMonths)
  return t.toISOString().slice(0, 7)
}

export const statusLabel: Record<string, string> = {
  scheduled: 'Planlandı', detected: 'Tespit edildi', confirmed: 'Onaylandı', assigned: 'Ekip atandı',
  in_progress: 'Çalışılıyor', resolved: 'Çözüldü', cancelled: 'İptal', issued: 'Kesildi', paid: 'Ödendi',
  settled: 'Mahsup edildi', open: 'Açık', dismissed: 'Yanlış alarm', available: 'Müsait', busy: 'Görevde', off_duty: 'İzinde',
  estimated: 'Tahmini', actual: 'Gerçek', measured: 'Ölçülen', interpolated: 'İnterpolasyon', corrected: 'Düzeltilmiş',
  reset: 'Sıfırlama', meter_change: 'Sayaç değişimi', sudden_drop: 'Ani düşüş', night_usage: 'Gece tüketimi',
  residential: 'Mesken', commercial: 'Ticarethane', industrial: 'Sanayi', agricultural: 'Tarım',
  transformer: 'Trafo', feeder: 'Besleyici', pole: 'Direk', unplanned: 'Plansız', planned: 'Planlı',
}
export const label = (s?: string | null) => (s ? statusLabel[s] ?? s : '—')

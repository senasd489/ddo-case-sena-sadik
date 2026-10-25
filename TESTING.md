# TESTING — EnerjiHat nasıl doğrulanır

## Tek komut

```bash
cp .env.example .env
make verify          # = docker compose up --build -d → sağlık bekle → docker compose --profile verify run --rm verify
```

`verify` konteyneri ayağa kalkmış ve seed edilmiş sistemi **yalnızca HTTP API üzerinden** dener (arayüz için de `index.html`, `/health` vekili ve SPA derin bağlantısı kontrol edilir), her kimlik için `PASS/FAIL` tablosu basar, hata varsa sıfırdan farklı kodla çıkar. Süre: ilk derleme hariç **~10 sn** (yerelde 5,6 sn ölçüldü). Tekrarlanabilirlik: her çalıştırma kendi abonesini/anahtarlarını üretir; `make reset-demo` verileri ilk seed haline döndürür.

```
| Kimlik | Özellik | Sonuç | ...
| HEALTH | GET /health (veritabanı dahil) | PASS | ...
...
Toplam 25 kontrol, 25 PASS, 0 FAIL — 5.6 sn
```

## Demo hesapları (parola hepsi için `Demo1234!`)

| Rol | E-posta | Not |
|---|---|---|
| Abone | `abone@enerjihat.local` | Abone #1 (AB00000001) |
| Abone (2) | `abone2@enerjihat.local` | Abone #2 — satır düzeyi yetki denemeleri için |
| Saha ekibi | `saha@enerjihat.local` | EKIP-01'e bağlı |
| Faturalama uzmanı | `fatura@enerjihat.local` | |
| Şebeke operatörü | `operator@enerjihat.local` | |
| Analist | `analist@enerjihat.local` | |
| Yönetici | `admin@enerjihat.local` | |
| Makineden makineye | `X-Ingest-Token: $INGEST_TOKEN` (.env) | simülatör / head-end / çağrı merkezi |

## Kimlik bazında: nasıl denenir / beklenen sonuç

| Kimlik | Nasıl denenir (verify ne yapıyor) | Beklenen sonuç | Elle deneme |
|---|---|---|---|
| **HEALTH** | `GET /health` | 200, `database.status=up`; DB kapalıysa 503 `degraded` | `curl localhost:8080/health` |
| **AUTH-1** | Yanlış parola, bilinmeyen e-posta ile giriş | ikisi de 401 `invalid_credentials` (aynı cevap) | requests.http #1 |
| **AUTH-2** | Tokensız ve bozuk tokenla `/auth/me` | 401 `unauthorized` / `invalid_token` | |
| **AUTH-3** | Refresh → yeni token; eski refresh tekrar | 401 `refresh_reused`; aynı ailenin yeni token'ı da iptal | requests.http #2 |
| **RBAC-1** | 7 rol/uç kombinasyonu (abone→admin, analist→kesinti oluştur, operatör→analiz, saha→fatura, faturalama→anahtarlama, abone→okuma alma…) | hepsi 403 `forbidden` | |
| **RBAC-2** | Abone #1 → abone #2'nin kaydı / tüketimi / faturaları | 403 (fatura detayı 404) — kendi verisi 200 | |
| **RBAC-3** | Yanlış `X-Ingest-Token` | 401 `invalid_ingest_token` | |
| **F-1** | Faturalama yeni abone + sayaç kaydeder (Idempotency-Key ile); detayda besleme yolu | 201; abone `AB…` no, sayaç bağlı, yol trafo→…→direk; direk olmayan düğüme bağlama 422 | Arayüz: Aboneler → Yeni abone |
| **F-2** | 97 okuma (00:00–24:00), aynı partinin tekrarı, `:07` hizasız okuma | 97 yazıldı; tekrar partide `duplicates=11, inserted=0`; hizasız `rejected=1` | requests.http #5 |
| **F-3** | Günlük tüketim + puant + en yüksek 15 dk talep | 9600 Wh/gün, puant 2000 Wh (17–22), talep 400 W | Abone portalı grafikleri |
| **F-4** | 1–20 Eylül ölçülü → tahmini fatura; 50 TL ödeme; kalan günler gelir → gerçek fatura | tahmini (eksik günler `estimated`), sonra gerçek fatura son satır `settlement −5000 kr`, tüketim 345,6 kWh | Fatura detayı: günlük döküm |
| **F-5** | Ağaç + bir besleyicinin etki analizi | 25 trafo / 75 besleyici / 600 direk; etki = ağaçtaki alt ağaç sayısı | Arayüz: Şebeke |
| **F-6** | Kesinti listesi, detay, geçersiz durum geçişi | 200; geçersiz geçiş 409 `invalid_transition` | Arayüz: Kesintiler |
| **F-7** | Analizi çalıştır + trafo kayıp raporu | trafo dengeleri; en kayıplı TR-0001 (simülatördeki kaçaklar) | Arayüz: Kayıp-kaçak |
| **T-1** | Bir günde: 4 okumalık boşluk (501 Wh), parti içi tekrar, hatalı okuma + geriye dönük düzeltme, gün içinde sayaç değişimi (saha ekibi) | toplam **9541 Wh birebir**; 5 `interpolated`, 2 `meter_change`, ≥1 `corrected` slot | `make bench-t1` (10 M okuma) |
| **T-2** | Ekim önizlemesi (15 Ekim tarife değişikliği), aynı dönemin iki kez üretilmesi | Ekim faturası 2 parçaya bölünür (2 KDV satırı); yeniden üretim aynı id/tutar/`input_hash`, `created=false` | Abone portalı → Tahmini fatura |
| **T-3** | Direği kendi alt direğinin altına taşı; besleyiciyi yedek trafoya aktar ve geri al; alt düğümde çakışan planlı kesinti; SAIDI/SAIFI | döngü 409 `topology_cycle`; anahtarlama 200 (taşınan abone sayısı); çakışma 409 `planned_overlap`; göstergeler | Şebeke → düğüm seç → "Yedek beslemeye aktar" |
| **T-4** | Şüpheli liste (gerekçeli) + simülatör yer gerçeğine göre değerlendirme | `sudden_drop` ve `night_usage` işaretleri; precision ≥ 0,8, recall ≥ 0,8 (yerelde 1,00/1,00) | `make bench-t4` |
| **T-5** | 3 abone son nefes + 1 mükerrer arama → tek olay; onay; ekip ataması; tüm etkilenen abonelerden `restore` | tek kesinti, mükerrer işaretli, `confirmed`/`assigned`, restore sonrası `resolved` | Kesintiler → olay → Bildirimler |
| **RACE** | Aynı faturaya iki eşzamanlı %60'lık ödeme | biri 201, diğeri 409 `overpayment` | `go test -run Concurrent ./...` |
| **IDEMP** | Aynı anahtar + aynı gövde ×2, aynı anahtar + farklı gövde, anahtarsız ödeme | ikinci istek aynı gövde + `Idempotent-Replayed: true`; farklı gövde 422; ödeme 400 | requests.http #12 |
| **AUDIT** | Denetim kaydında işlemler | `invoice.issue`, `payment.create`, `topology.switch`, `meter.replace`, `subscriber.create` | Yönetici → Denetim |
| **OBS** | `/metrics`, `/openapi.yaml` | Prometheus metinleri, OpenAPI 3 | |
| **LIVE** | `/api/v1/live` son okuma partisi | < 20 dk önce (simülatör akıyor) | Üst bar yeşil nokta |
| **WEB** | `http://frontend:3000/`, `/health` vekili, `/subscribers/1` | 200 / 200 / 200 | `localhost:3000` |

## Zorlu görevlerin kanıt komutları

| Görev | Komut | Ne gösterir |
|---|---|---|
| **T-1** | `cd backend && go test -run 'Gap\|Duplicate\|Correction\|Reset\|MeterChange\|Midnight\|Idempotent\|Concurrent' -v ./internal/metering` | Her senaryoda Wh birebir; 4 işçi aynı kuyruğu çift işlemeden boşaltır |
| T-1 | `make bench-t1` (`METERS=104200` varsayılan) | ≈10 M okuma ayrı bir bench veritabanına; okuma/sn, slot/sn, `EXPLAIN ANALYZE` — çıktı: `docs/bench/t1.txt` |
| **T-2** | `cd backend && go test -v ./internal/billing` | Kademe/zaman dilimi/reaktif/vergi hesapları elle hesapla eşleşir; 14/31 gün bölmesi; deterministik özet; tahmini→gerçek mahsup; eşzamanlı üretim tek fatura; eşzamanlı ödeme aşılamaz |
| **T-3** | `cd backend && go test -v ./internal/grid` | Alt ağaç sayımları; döngü reddi (API ve doğrudan SQL); yedek besleme ile abonelerin geri gelmesi; planlı çakışma (5 durum + eşzamanlı); SAIDI/SAIFI elle hesapla eşleşir, pencere kırpma, besleyici filtresi |
| **T-4** | `make bench-t4` ve `cd backend && go test -v ./internal/loss` | Yerleştirilmiş 5 kaçak + 3 gece anomalisi + tuzaklar: filtresiz P=0,667 → filtreli P=1,0, R=1,0 — çıktı: `docs/bench/t4.txt` |
| **T-5** | `cd backend && go test -v ./internal/dispatch` | Kümeleme + LCA + mükerrer + otomatik onay; 20 eşzamanlı sinyal → 1 olay; 3 ekip/5 kesinti eşzamanlı → 3 atama, çift atama yok; restore ve okumalardan uzlaştırma ile kapanış |
| Tümü | `make verify` | 25 uçtan uca kontrol |
| Yük | `make load` | k6, 60 sn karışık trafik — `docs/bench/load.txt` |
| Kaos | `make chaos` | DB durur → 503 → döner → 200, akış devam; API SIGKILL → geri gelir |
| Dizin | `make explain` | dizinli / dizinsiz `EXPLAIN ANALYZE` — `docs/bench/explain.txt` |

Go testleri gerçek PostgreSQL ister: `TEST_DATABASE_URL=postgres://kullanıcı:parola@localhost:5432/postgres go test ./...` (her test şablon veritabanından ~50 ms'de kopyalanan kendi veritabanını kullanır). `TEST_DATABASE_URL` yoksa veritabanı testleri atlanır, birim testleri çalışır. CI'da PostgreSQL servisiyle hepsi çalışır.

## Arayüzde elle denenecek kısa senaryolar (`http://localhost:3000`)

1. **Canlılık** — herhangi bir hesapla gir: üst barda yeşil nokta, "Son parti: … sn önce", son 1 saatteki okuma/bildirim ve açık kesinti sayısı 10 sn'de bir değişir.
2. **Abone** (`abone@`) — Aboneliğim: günlük tüketim grafiğinde bir çubuğa tıkla → o günün 15 dk profili; turuncu çubuklar interpolasyonla doldurulan slotlar. "Tahmini fatura"da ay seç (Ekim → 15 Ekim tarife değişikliği). Kesinti varsa "Beslememdeki kesintiler"de tahmini düzelme saati.
3. **Operatör** (`operator@`) — Kesintiler: simülatörün 3 dakikada bir ürettiği arızalar burada belirir (bildirim sayısı artar, otomatik onay + ekip ataması, sayaçlar enerjilenince "Çözüldü"). Bir kesintiye tıkla → Bildirimler (son nefes / arama / mükerrer) ve durum geçmişi.
4. **Operatör** — Şebeke: haritada kırmızı = arıza noktası, siyah = enerjisiz. Bir besleyici seç → "Yedek beslemeye aktar" → etkilenen aboneler geri gelir; "Normal beslemeye dön".
5. **Operatör** — "Planlı kesinti": aynı besleyiciye çakışan iki planlı kesinti dene → ikincisi reddedilir.
6. **Saha ekibi** (`saha@`) — Görevim: atanmış iş varsa "Sahaya ulaştım" → "Arıza giderildi"; elle okuma girişi; sayaç değişimi formu.
7. **Faturalama** (`fatura@`) — Aboneler → bir abone → "Fatura üret" (geçen ay) → aynı butona tekrar bas: "Değişmedi (aynı girdi → aynı fatura)". Faturalar → Dönem faturalama.
8. **Abone** — Fatura detayında "Öde": aynı ödeme tekrar gönderilse bile tek kez alınır; fazla tutar 409.
9. **Analist** (`analist@`) — Kayıp-kaçak → "Analizi çalıştır": trafo dengesi grafiği (kırmızı = tolerans üstü), şüpheli kartlarında gerekçe metni; "Doğrula"/"Yanlış alarm". SAIDI/SAIFI ekranı.
10. **Yönetici** (`admin@`) — Denetim: yukarıdaki işlemlerin kim/ne/eski/yeni kayıtları; Kullanıcılar → yeni kullanıcı.
11. **Mobil** — tarayıcıyı 390 px'e daralt: menü ☰ ile açılır, tablolar yatay kayar.

## Elle API denemesi

`requests.http` (VS Code REST Client): tüm uçlar, demo hesaplarla. Önce dosyanın başındaki `@ingest` değişkenini `.env` içindeki `INGEST_TOKEN` ile eşleyin; giriş istekleri token'ları sonraki isteklere otomatik aktarır.

# Mimari Karar Kayıtları (ADR)

Her kayıt: **bağlam → karar → alternatifler → sonuçlar**.

---

## ADR-001 — Go standart kütüphane + chi, ORM yok, düz SQL + pgx

**Bağlam.** İşin ağırlığı SQL'de: recursive CTE (topoloji), pencere fonksiyonları (tüketim farkları), `EXCLUDE` kısıtları, `FOR UPDATE SKIP LOCKED`, `COPY`. Hepsi PostgreSQL'e özgü.

**Karar.** HTTP katmanında yalnızca `chi` yönlendiricisi, veritabanında `pgx/v5` ile elle yazılmış SQL. Her paket kendi sorgularını taşır (`metering`, `billing`, `grid`, `dispatch`, `loss`).

**Alternatifler.** GORM/ent: CTE, `COPY` ve kilit cümleleri ORM dışına taşar, sorgu planını görmek zorlaşır. `sqlc`: güzel ama `unnest` dizileri ve dinamik `COPY` ile kod üretimi akışı bu süreye sığmadı.

**Sonuçlar.** Sorgular `EXPLAIN ANALYZE` ile birebir ölçülebildi (docs/bench). Tip güvenliği testlerle sağlanıyor; derleme zamanında SQL kontrolü yok.

---

## ADR-002 — Okumalar: günlük RANGE partisyon, `COPY → geçici tablo → INSERT … ON CONFLICT`

**Bağlam.** Günde ~10–20 milyon okuma; tekrarlar, geç gelen ve geriye dönük düzeltilen okumalar var.

**Karar.** `readings (meter_id, read_at)` birincil anahtarlı, `read_at` üzerinden **günlük** partisyonlu. Parti önce `COPY` ile geçici tabloya, oradan tek `INSERT … ON CONFLICT DO UPDATE … WHERE değer farklıysa` ile ana tabloya. Aynı değer = tekrar (sayılır, yazılmaz), farklı değer = düzeltme (eski değer `reading_corrections`'a, `revision+1`). Partisyonlar arka plan işinde 120 gün geriye/3 gün ileriye önceden açılır.

**Alternatifler.** TimescaleDB (zorunlu yığın PostgreSQL 16; eklenti bağımlılığı istemedim); satır satır INSERT (100× yavaş); aylık partisyon (silme/arşivleme kabası büyük, tek günün yeniden hesaplanması daha çok sayfa okur).

**Sonuçlar.** 10,09 M okuma 98 sn'de (≈103 bin/sn, 2 vCPU). Yük testi iki dersi gösterdi: (1) `CREATE TABLE … PARTITION OF` üst tabloda kısa bir `ACCESS EXCLUSIVE` kilit alır — sıcak yolda yapılırsa tüm okumalar bekler → partisyonlar önceden açılıyor; (2) sabit zaman sınırı olmayan join'ler 120+ partisyonun hepsine bakar → sorgulara sabit `read_at BETWEEN $min AND $max` eklendi (plan anında budama).

---

## ADR-003 — Tüketim hesabı: kirli sayaç-gün kuyruğu + tek set-tabanlı SQL (pencere fonksiyonları)

**Bağlam.** Bir okuma; o günün, bir önceki günün (gece yarısı sınırı) ve bir boşluğu dolduruyorsa aradaki günlerin tüketimini değiştirir.

**Karar.** Okuma alma, etkilenen (sayaç, yerel gün) çiftlerini `consumption_dirty`'ye yazar. İşçi bu kuyruğu üç kısa işlemle işler: **talep et** (`UPDATE … SET claimed_at` + `FOR UPDATE SKIP LOCKED`) → **hesapla** (sayaç başına advisory lock altında) → **sil** (arada yeniden işaretlenmediyse). Hesap tek SQL: bilinen noktalar (gün içi okumalar + günün öncesindeki/sonrasındaki en yakın okuma + sayaç takma/sökme sentetik noktaları) → `LAG()` → segment farkı → `generate_series` ile k slota **tam sayı** bölme `floor(d·i/k) − floor(d·(i−1)/k)` (Wh kaybolmaz). Endeks yarıdan fazla düştüyse sıfırlama, daha az düştüyse veri hatası (0, `corrected`).

**Alternatifler.** Okuma anında senkron hesap (ingest gecikir, geç gelen veri her şeyi tekrar hesaplatır); Go'da satır satır hesap (10 M satırı ağdan çekmek); tek uzun işlemli `DELETE … RETURNING` kuyruğu — ilk sürüm buydu, yük testinde okuma alma işçinin işlemini 20 sn bekledi → üç kısa işleme bölündü.

**Sonuçlar.** 10 M slot ≈ 100 sn; yeniden hesap idempotent (test). Ödünleşim: gün içi okumanın tüketime yansıması işçi aralığı kadar (2 sn) gecikir.

---

## ADR-004 — Para: kuruş `bigint`, birim fiyat `numeric`, motor `math/big.Rat`, satır başına tek yuvarlama

**Bağlam.** "Aynı dönemin faturası yeniden üretildiğinde sonuç değişmemeli." Float yasak.

**Karar.** Fatura motoru (`billing/engine.go`) veritabanından bağımsız saf bir fonksiyon: günlük tüketim + tarife sürümleri + önceki tahmini fatura → satırlar. Tüm ara hesaplar tam kesirli (`big.Rat`); her satır **bir kez** yarım-yukarı kuruşa yuvarlanır, vergiler yuvarlanmış satır tutarları üzerinden hesaplanır. Girdilerin kanonik JSON'unun SHA-256'sı (`input_hash`) faturayla saklanır: aynı girdi → aynı fatura (200, `created=false`); girdi değiştiyse 409, `reissue=true` ile eski fatura iptal + yeni revizyon (denetim kaydıyla).

**Alternatifler.** `numeric` ile SQL'de hesap (gün esaslı kademe bölmesi ve satır üretimi okunaksızlaşır); `float64` (yasak ve yanlış).

**Sonuçlar.** Motor %100'e yakın birim test kapsamında; dönem ortası tarife değişikliği (15 Ekim) kademe sınırlarını gün oranında küçültür (150 kWh × 14/31 = 67,742).

---

## ADR-005 — Tahmini fatura mahsubu: yalnızca *ödenen* tutar mahsup edilir

**Bağlam.** Tahmini fatura kesildikten sonra gerçek okuma gelir.

**Karar.** Gerçek fatura dönemin tamamını yeniden hesaplar ve tahmini faturaya **ödenmiş** tutarı `settlement` satırıyla düşer; tahmini fatura `settled` olur (artık ödenemez). Mahsup edilen tahmini fatura girdinin parçasıdır, yani yeniden üretim yine aynı sonucu verir.

**Alternatif.** Tahmini faturanın tamamını düşmek: ödenmemiş bir tahmin "alacak" gibi görünür ve abone ödemediği tutarı geri almış olur.

---

## ADR-006 — Eşzamanlılık: her paylaşılan durum için en dar kilit

| Durum | Mekanizma | Kanıt |
|---|---|---|
| Ödeme (bakiye) | `SELECT … FOR UPDATE` fatura satırı | `TestConcurrentPaymentsNeverOverpay`, verify `RACE` |
| Aynı abone-dönem faturası | `pg_advisory_xact_lock(abone)` + kısmi unique index | `TestConcurrentGenerateCreatesOneInvoice` |
| Ekip ataması | `FOR UPDATE SKIP LOCKED` en yakın ekip + `crew_one_active_job` kısmi unique index | `TestConcurrentDispatchNeverDoubleBooks` (3 ekip/5 kesinti) |
| Kesinti kümeleme | besleyici başına advisory lock | `TestConcurrentSignalsCreateExactlyOneOutage` (20 sinyal → 1 olay) |
| Planlı kesinti çakışması | trafo başına advisory lock + ata/çocuk recursive kontrol + `EXCLUDE USING gist` | `TestConcurrentPlannedOutagesOnlyOneWins` |
| Topoloji döngüsü | tetikleyicide advisory lock + recursive kontrol | `TestSwitchRejectsCyclesAndWrongKinds` |
| Tüketim kuyruğu | `SKIP LOCKED` talep + sayaç başına advisory lock | `TestConcurrentWorkersDrainQueueOnce` |
| Idempotency-Key | `(user, key)` PK + `ON CONFLICT DO NOTHING` | `TestIdempotencyConcurrentSameKey` |

`SERIALIZABLE` + yeniden deneme yerine bunları seçtim çünkü her durumda çatışan anahtar biliniyor; seri hale getirme hatası yerine bekleme/atlama daha öngörülebilir. Deadlock (`40P01`) yük testinde bir kez görüldü (kuyruk satırlarının farklı sırada kilitlenmesi) → `ORDER BY meter_id, day` ile sabit sıra + yeniden deneme.

---

## ADR-007 — Topoloji: komşuluk listesi + recursive CTE, kurallar veritabanında

**Karar.** `network_nodes(parent_id)`; tür kuralları (besleyici→trafo, direk→besleyici/direk) ve döngü engeli `BEFORE UPDATE` tetikleyicisinde; abone yalnızca direğe bağlanır (bileşik FK `(pole_id, 'pole') → (id, kind)`). Ağaç tek recursive CTE ile gezilir, alt ağaç abone sayıları `unnest(path)` + `GROUP BY` ile tek geçişte. Anahtarlama `parent_id`'yi değiştirir ve açık kesintilerin etkilenen kümesini yeni topolojiye göre yeniden hesaplar (çıkan aboneler `restored_at = now()`).

**Alternatifler.** `ltree`/closure table: okuma hızlı ama anahtarlamada alt ağacın tüm yollarını yeniden yazmak gerekir; bu alanda topoloji sık değişir (anahtarlama) ve ağaç sığdır (≤ 10 seviye).

---

## ADR-008 — Kayıp-kaçak: robust istatistik + yanlış alarm filtreleri, açıklanabilir işaretler

**Karar.** Ani düşüş için **log ölçekte** medyan/MAD robust z (hane tüketimindeki dalgalanma çarpımsal); gece tüketimi için kategori akranlarına göre robust z. Yanlış alarm azaltma: (1) robust istatistik, (2) veri kalitesi kapısı (eksik/interpolasyonlu gün, < %98 tam trafo günü), (3) bağlam dışlaması (kesinti ve sayaç değişimi günleri), (4) kalıcılık (7 günün ≥ 3'ü), (5) çapraz kanıt (trafoda kalıcı fazla kayıp → `high`). Her işaret, okunabilir gerekçe ve kullanılan sayılarla saklanır; analist kararı denetlenir ve yeniden çalıştırmada korunur.

**Kanıt.** `make bench-t4`: filtresiz precision 0,667 → filtreli 1,0 (recall 1,0). İlk sürüm z'yi doğrusal ölçekte hesaplıyordu ve ±%60 dalgalanan iki kaçağı kaçırdı (recall 0,75) — log ölçeğe geçince düzeldi.

**Alternatif.** Isolation forest / ML: açıklanabilirlik ve bu süre için fazla; kural + robust istatistik saha ekibine "neden" sorusunun cevabını verir.

---

## ADR-009 — Kesinti kümeleme: besleyici + zaman penceresi, arıza yeri = en yakın ortak ata (LCA)

**Karar.** Sinyal (arama / son nefes) aynı besleyicide 15 dk içinde açılmış ya da aboneyi zaten etkileyen açık kesintiye bağlanır; yoksa yeni olay. Arıza yeri bildirim yapan direklerin LCA'sı (her yeni sinyalde yeniden hesaplanır, etkilenen küme güncellenir). 3 farklı abone → otomatik onay. Mükerrer: aynı abone aynı olayda ikinci kez. Kapanış: sayaçtan `restore` mesajı ya da — mesaj kaybolursa — etkilenen sayaçtan kesinti sonrası okuma gelmesi (akıllı sayaç enerjisizken okuma gönderemez).

**Sonuçlar.** Simülatör yeniden başladığında restore mesajları kayboluyor ve kesintiler sonsuza dek açık kalıyordu; okumalardan uzlaştırma bunu kapattı.

---

## ADR-010 — Bağımlılık minimizasyonu ve `vendor/`

**Bağlam.** Geliştirme ortamında `proxy.golang.org` ve Docker Hub'a erişim yoktu; teslimde "soğuk makinede 5 dakikada ayağa kalkma" şartı var.

**Karar.** Doğrudan bağımlılıklar yalnızca `chi`, `pgx`, `golang-jwt`, `x/crypto` (argon2id). Migrasyon için goose yerine 60 satırlık gömülü migratör (her dosya tek işlem, `pg_advisory_lock` ile tek uygulayıcı); Prometheus istemcisi yerine 200 satırlık metin formatı yazıcı. Bağımlılıklar `vendor/` altında: Docker derlemesi ağ istemez, önbelleksiz derleme hızlıdır.

**Sonuçlar.** goose'un CLI, versiyon geri alma (down) gibi özellikleri yok; migrasyonlar yalnızca ileri yönlü (`make reset-demo` şemayı baştan kurar).

---

## ADR-011 — Kimlik: argon2id + kısa ömürlü JWT + dönen, tek kullanımlık refresh token

**Karar.** Parola özeti argon2id (m=19 MiB, t=2, p=1 — OWASP). Access JWT 15 dk, HS256, `alg` sabit. Refresh token rastgele 256 bit, veritabanında yalnızca SHA-256'sı; her kullanımda döner; **iptal edilmiş token tekrar gelirse tüm aile iptal** (çalınma tespiti). Bilinmeyen e-postada da aynı maliyette doğrulama yapılır (zamanlama ile kullanıcı keşfi engellenir). Makineden makineye (head-end, çağrı merkezi) ayrı `X-Ingest-Token`; kullanıcı rolü verilmez.

**Yetki.** Her route kendi rol listesini `auth.Require(...)` ile belirtir (admin otomatik geçmez); satır düzeyi (abone yalnızca kendisi) sorgu tarafında `CanAccessSubscriber`. Başka abonenin faturası 404 döner (varlığı sızdırılmaz).

---

## ADR-012 — Frontend: Vue 3 + Vite + TypeScript, Chart.js; nginx ile tek origin

**Karar.** Vue 3 (Composition API) — şablon sözdizimi okunaklı, ekiplerin çoğu tanıyor, derleme hızlı. Durum için Pinia (yalnızca oturum), her ekranda tek tip `useAsync` (yükleniyor/hata/boş). Grafik: Chart.js (hafif, ağaç sallanabilir). Üretimde nginx-unprivileged statik dosyaları sunar ve `/api`'yi backend'e vekiller: tarayıcı tek origin görür, CORS gerekmez.

**Alternatifler.** React (daha fazla seçenek, bu ölçekte fark yok); HTMX (rol başına zengin grafik/ağaç ekranları için sınırlı).

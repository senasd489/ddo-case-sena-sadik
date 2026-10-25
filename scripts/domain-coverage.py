#!/usr/bin/env python3
"""Go kapsama profilinden paket bazında deyim kapsamı; çekirdek (domain)
paketlerin toplamı eşik altındaysa çıkış kodu 1. Kullanım:
  domain-coverage.py coverage.out [eşik=70]"""
import collections, re, sys

DOMAIN = ["metering", "billing", "grid", "dispatch", "loss"]
prof, thr = sys.argv[1], float(sys.argv[2]) if len(sys.argv) > 2 else 70
blocks = {}
for line in open(prof).read().splitlines()[1:]:
    m = re.match(r"(.+?):(\S+) (\d+) (\d+)", line)
    if not m or "/internal/" not in m.group(1):
        continue
    key = (m.group(1), m.group(2))
    n, c = int(m.group(3)), int(m.group(4))
    old = blocks.get(key, (n, 0))
    blocks[key] = (n, max(old[1], c))
tot, cov = collections.Counter(), collections.Counter()
for (f, _), (n, c) in blocks.items():
    pkg = f.split("/internal/")[1].split("/")[0]
    tot[pkg] += n
    cov[pkg] += n if c else 0
print("| Paket | Deyim kapsamı |\n|---|---|")
for p in sorted(tot):
    mark = " **(çekirdek)**" if p in DOMAIN else ""
    print(f"| {p}{mark} | {100 * cov[p] / tot[p]:.1f}% |")
t = sum(tot[p] for p in DOMAIN)
c = sum(cov[p] for p in DOMAIN)
pct = 100 * c / t
print(f"\n**Çekirdek paketler toplamı: {pct:.1f}%** (eşik %{thr:.0f})")
sys.exit(0 if pct >= thr else 1)

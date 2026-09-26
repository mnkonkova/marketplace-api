"""Довезти правила из grafana/alerts.yml в Grafana Cloud.

Запуск:

    export G=https://<ваш>.grafana.net
    export T=<service account token с правами на alerting>
    python3 scripts/sync-alerts.py

Токен берётся из окружения и никуда не пишется. После работы отзовите его:
он живёт в истории команд.

Файл в репозитории и то, что реально заведено, разошлись: в файле 50
правил, в Grafana 33. Восемнадцати нет вовсе — весь сбор статистики,
instacurl, напоминания. Правили файл, а в Grafana он не ехал: провижининга
нет, заводили руками.

Скрипт сверяет по ИМЕНИ и создаёт недостающие. Существующие НЕ ТРОГАЕТ:
у них могли быть правки в интерфейсе, и затирать их молча нельзя. То есть
это не «привести Grafana к файлу», а «довезти то, чего нет»; расхождения в
условиях сверяются глазами — скрипт их показывает, но не правит.

BotrabotDown в Grafana есть, а в файле его нет и не будет: он ходит
HTTP-датасорсом в чужой сервис, в promql это не выражается (см. запись в
самом alerts.yml).
"""
import json, os, subprocess, sys, yaml

G = os.environ["G"]; T = os.environ["T"]
FOLDER = "fjq6g6"          # та же папка, где лежат остальные
DS = "grafanacloud-prom"   # тот же источник

def api(method, path, body=None):
    cmd = ["curl", "-s", "-m", "30", "-X", method,
           "-H", f"Authorization: Bearer {T}", "-H", "Content-Type: application/json",
           "-H", "X-Disable-Provenance: true", f"{G}{path}"]
    if body is not None:
        cmd += ["-d", json.dumps(body, ensure_ascii=False)]
    out = subprocess.run(cmd, capture_output=True, text=True).stdout
    try:
        return json.loads(out)
    except Exception:
        return {"raw": out[:300]}

def rule_body(name, expr, dur, labels, ann, group):
    """Форма — копия уже заведённых правил: запрос → reduce(last) → порог >0.

    Порог именно «больше нуля», потому что все наши выражения уже
    сравнивают сами (`> 0.9`, `absent(...)`, `< 0.1`): в Grafana приезжает
    готовое 1/0, и второй порог здесь только чтобы правило было валидным.
    """
    return {
        "folderUID": FOLDER, "ruleGroup": group, "title": name, "condition": "C",
        "orgID": 1, "for": dur or "0s", "noDataState": "OK", "execErrState": "Error",
        "labels": labels or {}, "annotations": ann or {},
        "data": [
            {"refId": "A", "queryType": "", "relativeTimeRange": {"from": 600, "to": 0},
             "datasourceUid": DS,
             "model": {"expr": expr, "intervalMs": 1000, "maxDataPoints": 43200, "refId": "A"}},
            {"refId": "B", "queryType": "", "relativeTimeRange": {"from": 0, "to": 0},
             "datasourceUid": "__expr__",
             "model": {"expression": "A", "intervalMs": 1000, "maxDataPoints": 43200,
                       "reducer": "last", "refId": "B", "type": "reduce"}},
            {"refId": "C", "queryType": "", "relativeTimeRange": {"from": 0, "to": 0},
             "datasourceUid": "__expr__",
             "model": {"conditions": [{"evaluator": {"params": [0], "type": "gt"},
                                       "operator": {"type": "and"}, "type": "query"}],
                       "expression": "B", "intervalMs": 1000, "maxDataPoints": 43200,
                       "refId": "C", "type": "threshold"}},
        ],
    }

existing = {r["title"]: r for r in api("GET", "/api/v1/provisioning/alert-rules")}
doc = yaml.safe_load(open("grafana/alerts.yml"))

created, failed = [], []
for grp in doc["groups"]:
    for r in grp["rules"]:
        name = r["alert"]
        if name in existing:
            continue
        body = rule_body(name, r["expr"].strip(), r.get("for"),
                         r.get("labels"), r.get("annotations"), grp["name"])
        res = api("POST", "/api/v1/provisioning/alert-rules", body)
        if res.get("uid"):
            created.append(name)
        else:
            failed.append((name, json.dumps(res, ensure_ascii=False)[:200]))

print("создано:", len(created))
for n in created: print("  +", n)
if failed:
    print("не удалось:", len(failed))
    for n, err in failed: print("  -", n, err)

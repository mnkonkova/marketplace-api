#!/usr/bin/env python3
"""Конвертер Prometheus alert-rules (alerts.yml) в Grafana-managed alert format
и push через provisioning API.

Каждый Mimir-rule вида:
  alert: X
  expr: <complex_promql_with_comparison>
  for: 5m
  labels/annotations: ...

превращается в Grafana-managed:
  - A: Prometheus query с исходным expr (включая `> threshold`)
  - B: reduce last(A) — превращает time-series в scalar (или NoData если пусто)
  - C: threshold B > 0 — финальное условие, fire когда A что-то вернул

Логика: если Prometheus expr с `> threshold` уже отфильтровал, любой
ненулевой результат означает firing. NoData = OK.

Запуск:
  GRAFANA_URL=https://...grafana.net GRAFANA_TOKEN=glsa_... \\
    python3 scripts/convert-alerts.py grafana/alerts.yml
"""
import os, sys, json, urllib.request, urllib.error
import yaml

GRAFANA_URL = os.environ.get("GRAFANA_URL", "").rstrip("/")
GRAFANA_TOKEN = os.environ.get("GRAFANA_TOKEN", "")
FOLDER_UID = os.environ.get("FOLDER_UID", "fjq6g6")  # marketpclce
DS_UID = os.environ.get("DS_UID", "grafanacloud-prom")

assert GRAFANA_URL and GRAFANA_TOKEN, "GRAFANA_URL и GRAFANA_TOKEN обязательны"


def _api(path, method="GET", body=None):
    req = urllib.request.Request(
        f"{GRAFANA_URL}{path}",
        data=json.dumps(body).encode() if body else None,
        method=method,
        headers={
            "Authorization": f"Bearer {GRAFANA_TOKEN}",
            "Content-Type": "application/json",
            "X-Disable-Provenance": "true",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            data = r.read()
            return r.status, json.loads(data) if data else None
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


def to_grafana_rule(mimir_rule: dict, group_name: str) -> dict:
    return {
        "title": mimir_rule["alert"],
        "ruleGroup": group_name,
        "folderUID": FOLDER_UID,
        "condition": "C",
        "for": mimir_rule.get("for", "0s"),
        "data": [
            {
                "refId": "A",
                "queryType": "",
                "relativeTimeRange": {"from": 600, "to": 0},
                "datasourceUid": DS_UID,
                "model": {
                    "expr": mimir_rule["expr"].strip(),
                    "intervalMs": 1000,
                    "maxDataPoints": 43200,
                    "refId": "A",
                },
            },
            {
                "refId": "B",
                "queryType": "",
                "relativeTimeRange": {"from": 0, "to": 0},
                "datasourceUid": "__expr__",
                "model": {
                    "type": "reduce",
                    "reducer": "last",
                    "expression": "A",
                    "refId": "B",
                },
            },
            {
                "refId": "C",
                "queryType": "",
                "relativeTimeRange": {"from": 0, "to": 0},
                "datasourceUid": "__expr__",
                "model": {
                    "type": "threshold",
                    "expression": "B",
                    "conditions": [
                        {
                            "type": "query",
                            "evaluator": {"type": "gt", "params": [0]},
                            "operator": {"type": "and"},
                        }
                    ],
                    "refId": "C",
                },
            },
        ],
        "noDataState": no_data_state(mimir_rule),
        "execErrState": "Error",
        "annotations": mimir_rule.get("annotations", {}),
        "labels": mimir_rule.get("labels", {}),
    }


def no_data_state(rule: dict) -> str:
    """Что делать, когда запрос не вернул данных.

    Всегда OK — и это не лень, а форма правил. Выражения вида
    `up{...} < 1` в Prometheus ФИЛЬТРУЮТ: пока сервис жив и up = 1,
    запрос возвращает пустоту. «Нет данных» здесь и есть нормальное
    здоровое состояние.

    2026-08-19 я переключил такие правила на Alerting, рассуждая, что
    пропажа метрик обязана звонить. Результат: APIDown, APINotScraped и
    N8nDown загорелись на полностью исправном проде и звонили, пока я не
    заметил. Ложная тревога хуже молчания — на неё перестают смотреть.

    Пропажу метрик ловит не noDataState, а отдельное правило
    APINotScraped с absent(): оно возвращает 1 РОВНО когда серии нет,
    то есть само превращает отсутствие данных в значение. Механизм в
    репозитории уже был, я его просто не разглядел.
    """
    return "OK"


def delete_existing_in_namespace(namespace="marketpclce"):
    """Удаляет все Mimir-rules в namespace (чтобы переехать полностью)."""
    code, body = _api(
        f"/api/ruler/grafanacloud-prom/api/v1/rules/{namespace}", method="GET"
    )
    if code != 200 or not body:
        return
    for group_name in body.keys() if isinstance(body, dict) else []:
        code, _ = _api(
            f"/api/ruler/grafanacloud-prom/api/v1/rules/{namespace}/{group_name}",
            method="DELETE",
        )
        print(f"  rm mimir {group_name}: HTTP {code}")


def delete_existing_grafana_managed(folder_uid):
    """Удаляет все Grafana-managed rules в folder перед re-import'ом,
    чтобы не плодить дубли при повторных запусках. Сохраняет isPaused
    flag по-title для последующего восстановления."""
    code, body = _api("/api/v1/provisioning/alert-rules", method="GET")
    if code != 200 or not isinstance(body, list):
        return {}
    paused = {}
    deleted = 0
    for r in body:
        if r.get("folderUID") == folder_uid:
            if r.get("isPaused"):
                paused[r["title"]] = True
            _api(f"/api/v1/provisioning/alert-rules/{r['uid']}", method="DELETE")
            deleted += 1
    print(f"  removed {deleted} existing Grafana-managed rules in folder {folder_uid}")
    if paused:
        print(f"  was-paused: {list(paused.keys())}")
    return paused


def main(fp):
    with open(fp) as f:
        data = yaml.safe_load(f)

    print(f"== Delete existing Mimir rules in 'marketpclce' namespace ==")
    delete_existing_in_namespace("marketpclce")

    print(f"\n== Delete existing Grafana-managed rules in folder ==")
    paused = delete_existing_grafana_managed(FOLDER_UID)

    print(f"\n== Create Grafana-managed rules ==")
    ok = fail = 0
    for grp in data["groups"]:
        for r in grp.get("rules", []):
            if "alert" not in r:
                continue
            rule = to_grafana_rule(r, grp["name"])
            if r["alert"] in paused:
                rule["isPaused"] = True
            code, resp = _api(
                "/api/v1/provisioning/alert-rules", method="POST", body=rule
            )
            if code in (200, 201, 202):
                ok += 1
                print(f"  ok  {grp['name']:35s} {r['alert']}")
            else:
                fail += 1
                resp_str = (
                    json.dumps(resp)[:200] if isinstance(resp, dict) else str(resp)[:200]
                )
                print(f"  err {grp['name']:35s} {r['alert']}: HTTP {code} {resp_str}")
    print(f"\n{ok}/{ok+fail} alerts created")


if __name__ == "__main__":
    main(sys.argv[1] if len(sys.argv) > 1 else "grafana/alerts.yml")

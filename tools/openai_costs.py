"""What OpenAI billed in the last days: by project, by line item and by day.

Reads OPENAI_ADMIN_KEY from local/secrets.h (platform.openai.com -> Settings -> Organization ->
Admin keys; the normal API key may not read costs) and asks OpenAI's Costs API
(GET /v1/organization/costs). The output is German: the user reads it.

    python tools/openai_costs.py            # last 30 days
    python tools/openai_costs.py --days 7
"""

import argparse
import collections
import datetime
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

SECRETS_H = os.path.join(os.path.dirname(__file__), "..", "local", "secrets.h")
API = "https://api.openai.com/v1/organization/"
SHOWN = 0.005  # smaller amounts are left out of the lists


def secret(name):
    with open(SECRETS_H, encoding="utf-8") as file:
        match = re.search(rf'^#define {name}\s+"([^"]*)"', file.read(), re.M)
    return match.group(1) if match else ""


def get(key, path, query):
    request = urllib.request.Request(API + path + "?" + urllib.parse.urlencode(query, doseq=True),
                                     headers={"Authorization": "Bearer " + key})
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        sys.exit(f"OpenAI antwortet mit HTTP {error.code}: {error.read().decode(errors='replace')}")


def costs(key, days):
    """All daily buckets of the last `days` days, grouped by project and line item."""
    query = {"start_time": int(time.time()) - days * 86400, "bucket_width": "1d", "limit": min(days + 1, 180),
             "group_by": ["project_id", "line_item"]}
    buckets = []
    while True:
        answer = get(key, "costs", query)
        buckets += answer.get("data", [])
        if not answer.get("has_more") or not answer.get("next_page"):
            return buckets
        query["page"] = answer["next_page"]


def project_names(key):
    names, query = {}, {"limit": 100, "include_archived": "true"}
    while True:
        answer = get(key, "projects", query)
        names.update({p["id"]: p.get("name") or p["id"] for p in answer.get("data", [])})
        if not answer.get("has_more") or not answer.get("last_id"):
            return names
        query["after"] = answer["last_id"]


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--days", type=int, default=30)
    args = parser.parse_args()
    sys.stdout.reconfigure(encoding="utf-8")  # umlauts also when piped (Windows defaults to cp1252)
    key = secret("OPENAI_ADMIN_KEY")
    if not key:
        sys.exit("OPENAI_ADMIN_KEY zuerst in local/secrets.h eintragen (OpenAI -> Organization -> Admin keys).")

    by_item, by_day = collections.Counter(), collections.Counter()
    by_project = collections.defaultdict(collections.Counter)
    currency = "usd"
    for bucket in costs(key, args.days):
        day = datetime.date.fromtimestamp(bucket["start_time"])
        for result in bucket.get("results", []):
            amount = float(result.get("amount", {}).get("value") or 0)
            currency = result.get("amount", {}).get("currency", currency)
            item = result.get("line_item") or "(ohne Posten)"
            by_item[item] += amount
            by_day[day] += amount
            by_project[result.get("project_id") or "(ohne Projekt)"][item] += amount
    names = project_names(key)

    total = sum(by_item.values())
    unit = "$" if currency.lower() == "usd" else currency.upper() + " "
    print(f"OpenAI-Kosten der letzten {args.days} Tage: {unit}{total:.2f}"
          f" (im Schnitt {unit}{total / max(args.days, 1):.2f} pro Tag)\n")
    print("Nach Projekt (API-Keys gehören zu einem Projekt):")
    for project, items in sorted(by_project.items(), key=lambda p: -sum(p[1].values())):
        print(f"  {unit}{sum(items.values()):8.2f}  {names.get(project, project)}")
        for item, amount in items.most_common():
            if amount >= SHOWN:
                print(f"      {unit}{amount:8.2f}  {item}")
    print("\nNach Posten:")
    for item, amount in by_item.most_common():
        if amount >= SHOWN:
            print(f"  {unit}{amount:8.2f}  {100 * amount / total if total else 0:5.1f} %  {item}")
    print("\nNach Tag:")
    for day in sorted(by_day):
        print(f"  {day:%a %d.%m.}  {unit}{by_day[day]:7.2f}")


if __name__ == "__main__":
    main()

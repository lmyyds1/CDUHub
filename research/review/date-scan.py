#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""date-scan.py -- 全量日期质量扫描 (data/snapshot.json)

只读 data/snapshot.json，不写任何被跟踪的数据文件。
用法:
    python -X utf8 research/review/date-scan.py            # 人类可读报告 -> stdout
    python -X utf8 research/review/date-scan.py --json out.json
    python -X utf8 research/review/date-scan.py --section titles

设计: 逐条 article 归类到若干互不排斥的"异常类"，每类保留 URL 证据清单，
便于直接对照「修复日期识别」的改动。
"""

import argparse
import collections
import datetime as dt
import json
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
SNAPSHOT = os.path.join(ROOT, "data", "snapshot.json")

# ---------------------------------------------------------------- 正则
# 源站里出现过的完整日期形态（含分隔符混用/无分隔）:
FULL_DATE_RE = re.compile(
    r"(?<!\d)((?:19|20)\d{2})\s*[-/.年]\s*(\d{1,2})\s*[-/.月]\s*(\d{1,2})\s*日?(?!\d)"
)
# 形如 "2023年4月" / "2023-04" / "2023.4" —— 只有年月
YEAR_MONTH_RE = re.compile(r"(?<!\d)((?:19|20)\d{2})\s*[-/.年]\s*(\d{1,2})\s*月?(?!\d)")
# 只有月日（无年份）: "3.15" "3-15" "3/15" "3月15日"
MONTH_DAY_RE = re.compile(
    r"(?<![\d.])((?:0?[1-9]|1[0-2]))\s*[-/.月]\s*((?:0?[1-9]|[12]\d|3[01]))\s*日?(?![\d.])"
)
# 奇怪的前缀日期: "22 2023-04" 之类（日 年-月）
SPLIT_DATE_RE = re.compile(r"(?:^|\s)(\d{1,2})\s+((?:19|20)\d{2})\s*[-/.年]\s*(\d{1,2})(?:月|\s|$)")

YEAR_LIKE_RE = re.compile(r"(?<!\d)((?:19|20)\d{2})(?!\d)")


def shape(s):
    """把 dateText 归一成形状签名: 数字->9。"""
    return re.sub(r"\d", "9", s or "")


def iter_full_dates(text):
    """返回 text 中所有 '完整日期' 三元组 + 匹配片段。"""
    for m in FULL_DATE_RE.finditer(text or ""):
        try:
            yield int(m.group(1)), int(m.group(2)), int(m.group(3)), m.group(0)
        except ValueError:
            continue


def iso(y, m, d):
    try:
        return dt.date(y, m, d).isoformat()
    except ValueError:
        return None


def load():
    with open(SNAPSHOT, encoding="utf-8") as f:
        snap = json.load(f)
    return snap


def parse_date(s):
    """把 article['date'] 解析为 date 或 None，并记录是否语法合法。"""
    if not s:
        return None
    m = re.fullmatch(r"(\d{4})-(\d{2})-(\d{2})", s)
    if not m:
        return None
    y, mo, d = (int(g) for g in m.groups())
    try:
        return dt.date(y, mo, d)
    except ValueError:
        return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--json", help="把结构化报告写到该路径")
    ap.add_argument("--section", default="all")
    ap.add_argument("--limit", type=int, default=40, help="每类打印的 URL 条数")
    args = ap.parse_args()

    snap = load()
    arts = snap["articles"]
    sources = {s["id"]: s for s in snap["sources"]}
    last_updated = snap.get("lastUpdated", "")
    try:
        cutoff = dt.datetime.fromisoformat(last_updated).date()
    except ValueError:
        cutoff = dt.date(2026, 9, 14)

    src_name = {sid: s.get("name", sid) for sid, s in sources.items()}

    # ---- 每条 article 的派生事实
    rows = []
    for a in arts:
        d = parse_date(a.get("date", ""))
        dv = d.isoformat() if d else ""
        txt = a.get("dateText", "") or ""
        title = a.get("title", "") or ""
        content = a.get("content", "") or ""
        row = {
            "id": a.get("id", ""),
            "url": a.get("url", ""),
            "sourceId": a.get("sourceId", ""),
            "source": a.get("sourceName") or src_name.get(a.get("sourceId", ""), ""),
            "channel": a.get("channel", ""),
            "title": title,
            "date": a.get("date", "") or "",
            "dateOk": d is not None,
            "dateVal": d,
            "dateText": txt,
            "shape": shape(txt),
            "inferred": bool(a.get("dateInferred")),
            "content": content,
        }
        # title 内的完整日期
        tdates = list(iter_full_dates(title))
        row["titleDates"] = [(iso(y, m, dd) or "", frag) for (y, m, dd, frag) in tdates]
        # title 内"月日"无年份
        row["titleMonthDay"] = [
            (int(m.group(1)), int(m.group(2)), m.group(0))
            for m in MONTH_DAY_RE.finditer(title)
        ]
        # content 内完整日期（去掉 dateText 自身之后的第一个）
        row["contentDates"] = [
            (iso(y, m, dd) or "", frag) for (y, m, dd, frag) in list(iter_full_dates(content))[:6]
        ]
        row["contentHasFullDate"] = bool(row["contentDates"])
        rows.append(row)

    by_shape = collections.Counter(r["shape"] for r in rows)

    # ---------------------------------------------------------------- 分类
    def keep(pred):
        return [r for r in rows if pred(r)]

    cat = collections.OrderedDict()

    # 1) 无日期
    no_date = keep(lambda r: not r["dateVal"])
    cat["no_date_total"] = no_date
    cat["no_date_empty_text"] = [r for r in no_date if r["dateText"] == ""]
    cat["no_date_partial_text"] = [r for r in no_date if r["dateText"] != ""]

    # 2) date 非 YYYY-MM-DD / 非法日历日
    malformed = keep(lambda r: bool(r["date"]) and not r["dateOk"])
    cat["malformed_date"] = malformed

    # 3) 早于 2010
    early = keep(lambda r: r["dateVal"] and r["dateVal"].year < 2010)
    cat["earlier_than_2010"] = early

    # 4) 晚于采集日
    future = keep(lambda r: r["dateVal"] and r["dateVal"] > cutoff)
    cat["future_of_cutoff"] = future

    # 5) date 与 dateText 年份不一致
    def year_mismatch(r):
        if not r["dateVal"]:
            return False
        years = {int(y) for y in YEAR_LIKE_RE.findall(r["dateText"])}
        if not years:
            return False
        return r["dateVal"].year not in years

    cat["date_vs_text_year_mismatch"] = keep(year_mismatch)

    # 6) dateText 无年份（月日型）
    cat["dateText_no_year"] = [
        r for r in rows if r["dateText"] and not YEAR_LIKE_RE.search(r["dateText"])
    ]

    # 7) 标题含完整日期且 date 恰等于标题日期（标题污染候选）
    def title_equal(r):
        if not r["dateVal"]:
            return False
        return any(dd == r["dateVal"].isoformat() for dd, _ in r["titleDates"])

    cat["date_equals_title_date"] = keep(title_equal)

    # 8) 标题含完整日期但 date 不等于标题日期（标题日期存在但未被采用）
    def title_diff(r):
        if not r["dateVal"]:
            return False
        ds = [dd for dd, _ in r["titleDates"] if dd]
        return bool(ds) and r["dateVal"].isoformat() not in ds

    cat["date_differs_from_title_date"] = keep(title_diff)

    # 9) dateText 与 title 完全相同（解析字段被标题文本污染）
    cat["dateText_equals_title"] = [
        r for r in rows if r["dateText"] and r["dateText"].strip() == r["title"].strip()
    ]

    # 10) dateText 是标题 / 摘要的子串但长度异常（>20 字符，不是纯日期）
    cat["dateText_too_long"] = [r for r in rows if len(r["dateText"]) > 20]

    # 11) content 含完整日期但 date 不在其中（摘要日期污染候选）
    def content_only(r):
        if not r["dateVal"]:
            return False
        ds = [dd for dd, _ in r["contentDates"] if dd]
        return bool(ds) and r["dateVal"].isoformat() not in ds

    cat["content_date_differs"] = keep(content_only)

    # 11.1) dateText 乱序：形如「日 年-月」（如 `14 2026-09`），date 本身正确
    GARBLED_RE = re.compile(r"^\d{1,2}\s+(?:19|20)\d{2}")
    cat["dateText_garbled_order"] = [
        r for r in rows if GARBLED_RE.match(r["dateText"] or "")
    ]

    # 11.2) date 为空但标题末尾带完整日期（可从标题回填的候选）
    TITLE_TAIL_DATE_RE = re.compile(
        r"[\s\[\(（]?((?:19|20)\d{2})[-/.年](\d{1,2})[-/.月](\d{1,2})\s*日?[\s\]\)）]?$"
    )

    def title_tail_recoverable(r):
        if r["dateVal"]:
            return False
        m = TITLE_TAIL_DATE_RE.search(r["title"] or "")
        if not m:
            return False
        return iso(int(m.group(1)), int(m.group(2)), int(m.group(3))) is not None

    cat["no_date_but_title_has_date"] = keep(title_tail_recoverable)

    # 11.3) 导航/分页控件被当成文章
    NAV_TITLES = {"下页", "尾页", "首页", "上一页", "下一页", "更多", "more +", "校园公告", "新闻动态", "教学动态", "党建动态", "教育科研"}
    cat["nav_control_as_article"] = [
        r for r in rows if (r["title"] or "").strip() in NAV_TITLES and r["dateText"]
    ]

    # 12) 来源内全部同一天（>=5 篇且唯一日期数 == 1）
    per_src_dates = collections.defaultdict(list)
    for r in rows:
        if r["dateVal"]:
            per_src_dates[r["sourceId"]].append(r["dateVal"])
    flat_srcs = []
    for sid, ds in per_src_dates.items():
        if len(ds) >= 5 and len(set(ds)) == 1:
            flat_srcs.append((sid, len(ds), ds[0].isoformat()))
    cat["single_date_sources"] = [{"sourceId": s, "n": n, "date": d} for s, n, d in flat_srcs]

    # 13) 每个来源的日期缺失率 / 异常率
    per_src = collections.defaultdict(lambda: collections.Counter())
    for r in rows:
        c = per_src[r["sourceId"]]
        c["total"] += 1
        if not r["dateVal"]:
            c["no_date"] += 1
            if r["dateText"] == "":
                c["no_date_empty_text"] += 1
            if r["dateText"] and not YEAR_LIKE_RE.search(r["dateText"]):
                c["no_date_partial"] += 1
        if r["dateText"] and not YEAR_LIKE_RE.search(r["dateText"]):
            c["partial"] += 1
        if r["dateVal"] and r["dateVal"].year < 2010:
            c["early"] += 1
        if r["dateVal"] and r["dateVal"] > cutoff:
            c["future"] += 1
        if year_mismatch(r):
            c["mismatch"] += 1
        if GARBLED_RE.match(r["dateText"] or ""):
            c["garbled"] += 1
        if title_tail_recoverable(r):
            c["title_recoverable"] += 1
        if not r["dateVal"] or GARBLED_RE.match(r["dateText"] or "") or (r["dateVal"] and (r["dateVal"].year < 2010 or r["dateVal"] > cutoff)):
            c["anomaly"] += 1

    # ---- 输出
    out = []
    P = out.append
    P("# 日期扫描原始报告")
    P("")
    P("snapshot: %d 篇 / %d 来源 / lastUpdated=%s" % (len(arts), len(snap["sources"]), last_updated))
    P("")
    P("## 一、总量与 dateText 形状")
    P("")
    P("| shape | 条数 | 有 date | 无 date |")
    P("|---|---:|---:|---:|")
    has_date_by_shape = collections.Counter(
        r["shape"] for r in rows if r["dateVal"]
    )
    for sh, n in by_shape.most_common():
        P("| `%s` | %d | %d | %d |" % (sh if sh else "(空)", n, has_date_by_shape[sh], n - has_date_by_shape[sh]))
    P("")
    P("dateInferred=true 条数: %d" % sum(1 for r in rows if r["inferred"]))
    P("")
    P("## 二、分类计数")
    P("")
    P("| 类别 | 条数 |")
    P("|---|---:|")
    for k, v in cat.items():
        if k == "single_date_sources":
            P("| %s | %d 个来源 |" % (k, len(v)))
        else:
            P("| %s | %d |" % (k, len(v)))
    P("")

    if args.section in ("all", "sources"):
        P("## 三、来源维度（按异常条数排序）")
        P("")
        P("| 来源 | 总篇 | 无 date | 月日无年份 | dateText 全空 | 乱序 dateText | 标题可回填 | <2010 | 未来 | 异常合计 | 异常率 |")
        P("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
        ranked = sorted(per_src.items(), key=lambda kv: -kv[1]["anomaly"])
        for sid, c in ranked:
            if c["anomaly"] < 5:
                continue
            P(
                "| %s | %d | %d | %d | %d | %d | %d | %d | %d | %d | %.0f%% |"
                % (
                    src_name.get(sid, sid),
                    c["total"],
                    c["no_date"],
                    c["partial"],
                    c["no_date_empty_text"],
                    c["garbled"],
                    c["title_recoverable"],
                    c["early"],
                    c["future"],
                    c["anomaly"],
                    100.0 * c["anomaly"] / c["total"],
                )
            )
        P("")

    if args.section in ("all", "titles"):
        # 标题日期交叉统计
        t_with = [r for r in rows if r["titleDates"]]
        t_md = [r for r in rows if r["titleMonthDay"]]
        P("## 四、标题/正文日期交叉")
        P("")
        P("- 标题含完整日期: %d 篇；其中 date 恰等于标题日期: %d 篇" % (len(t_with), len(cat["date_equals_title_date"])))
        P("- 标题含完整日期但 date 不等于标题日期: %d 篇" % len(cat["date_differs_from_title_date"]))
        P("- 标题含月日（无年份）形态: %d 篇" % len(t_md))
        P("- dateText 与 title 完全相同: %d 篇" % len(cat["dateText_equals_title"]))
        P("- dateText 长度 > 20（非纯日期）: %d 篇" % len(cat["dateText_too_long"]))
        P("- content 含完整日期且 date 不在其中: %d 篇" % len(cat["content_date_differs"]))
        P("")

    # 每类证据
    P("## 五、逐类真实 URL 证据")
    P("")
    for k, v in cat.items():
        if k.endswith("_sources"):
            continue
        P("### %s (n=%d)" % (k, len(v)))
        P("")
        for r in v[: args.limit]:
            P("- [%s] `%s` date=`%s` dateText=`%s` title=%s" % (r["source"] or "?", r["url"], r["date"], r["dateText"], (r["title"][:70] or "")))
        P("")

    report = "\n".join(out)
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    print(report)

    if args.json:
        payload = {
            "totals": {
                "articles": len(arts),
                "sources": len(snap["sources"]),
                "lastUpdated": last_updated,
                "dateInferredTrue": sum(1 for r in rows if r["inferred"]),
            },
            "counts": {k: (len(v) if not k.endswith("_sources") else len(v)) for k, v in cat.items()},
            "shapes": dict(by_shape.most_common()),
            "perSource": {
                src_name.get(sid, sid): dict(c) for sid, c in per_src.items()
            },
            "categories": {
                k: [
                    {
                        "url": r["url"],
                        "source": r["source"],
                        "date": r["date"],
                        "dateText": r["dateText"],
                        "title": r["title"],
                        "channel": r["channel"],
                    }
                    for r in v[: args.limit]
                ]
                for k, v in cat.items()
                if not k.endswith("_sources")
            },
            "singleDateSources": cat["single_date_sources"],
        }
        with open(args.json, "w", encoding="utf-8") as f:
            json.dump(payload, f, ensure_ascii=False, indent=1)
        print("\n[写出] %s" % args.json)


if __name__ == "__main__":
    main()

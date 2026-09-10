#!/usr/bin/env python3
"""klno 出站身份验收器。

只打本机预演网关。发送前必须由预演启动器用 iptables/独立 namespace 隔离网关，
并把上游指向 echo server；本脚本不负责建立该隔离。--selftest 不发送任何请求。

前提：预演库的目标账号必须是 device + 实验收敛【双开】——线协议投影
(applyCodexDeviceWireProfile) 只在双开时生效，单开跑出来的结果不代表 pro1。

时区断言（可选）：目标账号开了账号级「请求时区替换」(extra.codex_request_timezone) 后，
用 SUB2API_REHEARSAL_TIMEZONE 声明该账号代理出口时区（如 America/Los_Angeles）。
探针会故意发一个错的时区（SENTINEL_TZ），断言出站被改写成期望值、且日期与目标时区当天相符；
不设这个环境变量时，时区断言整体跳过（发送侧照常带探针时区，不做判定）。

跑法：
  python3 fp_probe.py --selftest      # 离线自检检查器与反例，不发任何请求
  SUB2API_REHEARSAL_API_KEY=... SUB2API_REHEARSAL_TIMEZONE=America/Los_Angeles python3 fp_probe.py

正式跑之前会先发一条"热身"请求：代理出口时区是按需异步探测的（结果缓存 6 小时，
请求路径上不等探测），不热身的话第一个用例必然落在冷缓存上、拿到未改写的时区。
热身只在正式计数之前发生，不参与捕获比对。

设计约束（上一版踩过的坑，逐条钉死）：
  1. 每个用例必须有对应捕获，数量和路径都要对上；没发出去 = 失败，不是跳过。
  2. curl 的退出码要检查，非 0 直接失败。
  3. 设备身份按端点声明各自的载体，不做「body 或 头，有一个就行」的兜底——
     那会掩盖正确载体缺失。
  4. 不用「存在才比较」：关系两端都在 required 里，缺任一端即失败。
  5. 契约集中在 CONTRACTS 与 WS_*_CONTRACT，升级协议时集中修改。

自检：python3 fp_probe.py --selftest —— 用构造的坏样本验证检查器确实会报错，
不连服务器。验收器本身也要被验收。
预演凭据从 SUB2API_REHEARSAL_API_KEY 读取，不写入仓库。
"""
import base64
import json
import os
import re
import socket
import struct
import subprocess
import sys
import time

GW = "http://127.0.0.1:18080"
KEY = os.environ.get("SUB2API_REHEARSAL_API_KEY", "")
# klno 请求时区替换的期望值：账号代理出口 IP 所在时区。留空 = 跳过时区断言。
EXPECTED_TZ = os.environ.get("SUB2API_REHEARSAL_TIMEZONE", "").strip()
# 探针故意发的"错误"时区（UTC+14，任何美区/欧区账号都不可能匹配）。被改写的实现必然让它消失。
SENTINEL_TZ = "Pacific/Kiritimati"
CAP = "/opt/s2a-rehearsal/echo/capture.jsonl"
UA = "codex-tui/0.153.4 (Mac OS 26.2.0; arm64) Apple_Terminal/466 (codex-tui; 0.153.4)"
INSTALL = "7f582abd-05d2-4a59-b4e5-ec1b733b4edc"
# 真实客户端把工具清单留在 body 的 client_metadata、从兼容头里剥掉
# （codex-rs core/src/responses_metadata.rs 的 compatibility_headers）。
# 探针主动塞进去，才能验证网关确实做了剥离，而不是恰好没人带。
TOOLS = ["shell", "apply_patch"]
# 真实客户端的 window_number 初值 0、每次 auto-compact 递增
# （codex-rs core/src/state/auto_compact_window.rs:51，window_id 形态
# format!("{thread_id}:{window_number}")）。探针故意发一个非初值：写死成 :0/:1 的
# 实现只有在探针恰好发同一个数时才会"通过"。
WINDOW_NUMBER = 3

# ── 端点契约 ───────────────────────────────────────────────────────────────
# device_carrier 取值：
#   header_install  独立 x-codex-installation-id 头（真实客户端只有 compact 这么发）
#   body_install    body.client_metadata["x-codex-installation-id"]
#   meta_install    出站 x-codex-turn-metadata 的 installation_id
# 列多个表示都必须存在且相等。
CONTRACTS = {
    "responses": {
        "required_headers": ["originator", "user-agent", "version", "session-id", "thread-id",
                             "x-client-request-id", "x-codex-window-id", "x-codex-turn-metadata"],
        "forbidden_headers": ["x-codex-installation-id", "session_id", "conversation_id"],
        "required_body": ["prompt_cache_key", "client_metadata.session_id",
                          "client_metadata.thread_id", "client_metadata.x-codex-installation-id",
                          "client_metadata.x-codex-window-id",
                          "client_metadata.x-codex-turn-metadata"],
        "device_carrier": ["body_install", "meta_install", "body_meta_install"],
        "relations": [
            ("h:session-id", "h:thread-id"),
            ("h:thread-id", "h:x-client-request-id"),
            ("h:x-codex-window-id", "expr:thread_window"),
            ("b:prompt_cache_key", "h:session-id"),
            ("b:client_metadata.session_id", "h:session-id"),
            ("b:client_metadata.thread_id", "h:thread-id"),
            ("b:client_metadata.x-codex-window-id", "h:x-codex-window-id"),
            ("m:session_id", "h:session-id"),
            ("m:thread_id", "h:thread-id"),
            ("m:window_id", "h:x-codex-window-id"),
            ("m:turn_id", "m:root_turn_id"),
            ("bm:session_id", "b:client_metadata.session_id"),
            ("bm:thread_id", "b:client_metadata.thread_id"),
            ("bm:window_id", "h:x-codex-window-id"),
            ("bm:window_number", "expr:window_number"),
            ("bm:context_window_id", "m:context_window_id"),
            ("bm:turn_id", "m:turn_id"),
            ("bm:root_turn_id", "m:root_turn_id"),
        ],
    },
    # 图片是自建 Responses body：只带设备身份，没有会话级 client_metadata，也没有缓存键。
    "images": {
        "required_headers": ["originator", "user-agent", "version", "session-id", "thread-id",
                             "x-client-request-id", "x-codex-window-id", "x-codex-turn-metadata"],
        "forbidden_headers": ["x-codex-installation-id", "session_id", "conversation_id"],
        "required_body": ["client_metadata.x-codex-installation-id"],
        "device_carrier": ["body_install", "meta_install"],
        "relations": [
            ("h:session-id", "h:thread-id"),
            ("h:thread-id", "h:x-client-request-id"),
            ("h:x-codex-window-id", "expr:thread_window"),
            ("m:session_id", "h:session-id"),
        ],
    },
    # compact 是唯一发独立安装头的端点（core/src/client.rs:646），且不发
    # x-client-request-id（构造链没有 stream_request 那一步），body 无 client_metadata、
    # 有 prompt_cache_key（codex-api/src/common.rs 的 CompactionInput）。
    "compact": {
        "required_headers": ["originator", "user-agent", "version", "session-id", "thread-id",
                             "x-codex-window-id", "x-codex-turn-metadata", "x-codex-installation-id"],
        "forbidden_headers": ["x-client-request-id", "session_id", "conversation_id"],
        "required_body": ["prompt_cache_key"],
        "forbidden_body": ["client_metadata"],
        "device_carrier": ["header_install", "meta_install"],
        "relations": [
            ("h:session-id", "h:thread-id"),
            ("h:x-codex-window-id", "expr:thread_window"),
            ("b:prompt_cache_key", "h:session-id"),
            ("m:session_id", "h:session-id"),
        ],
    },
    # 真实客户端在该端点只发 turn-metadata + originator（ext/web-search/src/tool.rs 的
    # search_request_headers），不发任何会话头；body.id 就是 session_id。
    "search": {
        "required_headers": ["originator", "user-agent", "version", "x-codex-turn-metadata"],
        "forbidden_headers": ["session-id", "thread-id", "x-client-request-id",
                              "x-codex-window-id", "x-codex-installation-id",
                              "session_id", "conversation_id", "openai-beta"],
        "required_body": ["id"],
        "device_carrier": ["meta_install"],
        "relations": [("b:id", "m:session_id")],
    },
}

# 所有端点通用：真实客户端在任何面都不发旧的 responses=experimental；
# 兼容头的 turn-metadata 必须剥掉工具清单，body 里的必须保留。
FORBIDDEN_BETA = "responses=experimental"


def sid(n):  # v7 形态，末段可区分
    return "01a07c73-e312-76e1-9054-e4665b8ee0a%d" % n


def turn_meta(s, window_number=WINDOW_NUMBER):
    return json.dumps({"installation_id": INSTALL, "session_id": s, "thread_id": s,
                       "turn_id": "01a07c73-e3a0-7ae1-baf0-ce1c532f019c",
                       "root_turn_id": "01a07c73-e3a0-7ae1-baf0-ce1c532f019c",
                       "window_id": "%s:%d" % (s, window_number),
                       "context_window_id": "01a07c73-e312-76e1-9054-e4722b79a205",
                       "window_number": window_number, "tool_namespaces_info": TOOLS},
                      separators=(",", ":"))


def codex_headers(s, relayed=False):
    """relayed=True 模拟 31.108 中继：连字符会话头被剥掉，只剩体内 client_metadata。"""
    h = {"originator": "codex-tui", "user-agent": UA, "version": "0.153.4",
         "x-codex-installation-id": INSTALL, "x-codex-window-id": "%s:%d" % (s, WINDOW_NUMBER),
         "x-codex-turn-metadata": turn_meta(s), "openai-beta": FORBIDDEN_BETA}
    if not relayed:
        h.update({"session-id": s, "thread-id": s, "x-client-request-id": s})
    return h


def meta(s, window_number=WINDOW_NUMBER):
    return {"session_id": s, "thread_id": s, "x-codex-installation-id": INSTALL,
            "x-codex-window-id": "%s:%d" % (s, window_number),
            "x-codex-turn-metadata": turn_meta(s, window_number)}


def env_context(timezone=SENTINEL_TZ, date="2026-06-20"):
    """真实客户端在会话开始（以及时区/日期变化）时插入的环境块：
    codex-rs core/src/session/world_state.rs + core/src/context/world_state/environment.rs。"""
    return ("<environment_context>\n  <current_date>%s</current_date>\n"
            "  <timezone>%s</timezone>\n  <network enabled=\"true\" />\n</environment_context>"
            % (date, timezone))


def resp_body(s, stream=True):
    return {"model": "gpt-5.4", "stream": stream, "prompt_cache_key": s,
            "client_metadata": meta(s),
            "input": [
                {"type": "message", "role": "user",
                 "content": [{"type": "input_text", "text": env_context()}]},
                {"type": "message", "role": "user", "content": "hi"},
            ]}


def build_cases():
    s1, s2, s3, s4, s5, s6 = (sid(i) for i in range(1, 7))
    return [
        {"label": "A 直连 SSE", "path": "/v1/responses", "contract": "responses",
         "upstream": "/backend-api/codex/responses", "expect_env_timezone": True,
         "body": resp_body(s1), "headers": codex_headers(s1)},
        {"label": "B 中继剥头 SSE", "path": "/v1/responses", "contract": "responses",
         "upstream": "/backend-api/codex/responses", "expect_env_timezone": True,
         "body": resp_body(s2), "headers": codex_headers(s2, relayed=True)},
        {"label": "C 非流式", "path": "/v1/responses", "contract": "responses",
         "upstream": "/backend-api/codex/responses", "expect_env_timezone": True,
         "body": resp_body(s3, stream=False), "headers": codex_headers(s3)},
        {"label": "D compact", "path": "/v1/responses/compact", "contract": "compact",
         "upstream": "/backend-api/codex/responses/compact",
         "body": {"model": "gpt-5.4", "stream": False, "prompt_cache_key": s4,
                  "input": [{"type": "message", "role": "user", "content": "hi"}]},
         "headers": codex_headers(s4)},
        {"label": "E 图片生成", "path": "/v1/images/generations", "contract": "images",
         "upstream": "/backend-api/codex/responses",
         "body": {"model": "gpt-image-2", "prompt": "a cat", "n": 1, "size": "1024x1024"},
         "headers": codex_headers(s5)},
        # 客户端声明的搜索位置：时区替换开启时，出站两份副本（standalone 的
        # settings.user_location、PAT 桥接的 tools[].user_location 与 prompt 里的 settings）
        # 都必须变成账号出口时区，而 city/country 保持客户端原值。
        {"label": "F alpha/search", "path": "/v1/alpha/search", "contract": "search",
         "upstream": "/backend-api/codex/alpha/search", "expect_search_timezone": True,
         "body": {"id": s6, "model": "gpt-5.4", "query": "x",
                  "settings": {"search_context_size": "medium",
                               "user_location": {"type": "approximate", "city": "Shanghai",
                                                 "country": "CN", "timezone": SENTINEL_TZ}}},
         "headers": codex_headers(s6)},
    ]


# ── 检查器（纯函数，可离线自检）─────────────────────────────────────────────

def hdr(row, name):
    for k, v in row.get("headers", []):
        if k.lower() == name.lower():
            return v
    return None


def jget(obj, dotted):
    cur = obj
    for part in dotted.split("."):
        if not isinstance(cur, dict) or part not in cur:
            return None
        cur = cur[part]
    return cur


def parse_meta(raw, label, path, problems):
    if raw is None:
        return None
    if isinstance(raw, dict):
        return raw
    # echo server 对超长字符串截断成 '...<len N>'，截断后 JSON 解析失败会让断言全部落空。
    # 必须显式区分「抓包截断」和「网关发了坏 JSON」。
    if isinstance(raw, str) and raw.endswith(">") and "...<len " in raw:
        problems.append("%s %s 被 echo server 截断，断言无法执行（调高 trim_body 上限）" % (path, label))
        return None
    try:
        parsed = json.loads(raw)
        if not isinstance(parsed, dict):
            problems.append("%s %s 必须是 JSON 对象" % (path, label))
            return None
        return parsed
    except Exception as e:
        problems.append("%s %s 解析失败：%s" % (path, label, e))
        return None


def resolve(token, ctx):
    """把关系表达式解析成 (值, 是否可用)。缺失一律视为不可用 → 由调用方报失败。"""
    kind, _, name = token.partition(":")
    if kind == "h":
        return hdr(ctx["row"], name)
    if kind == "b":
        return jget(ctx["body"], name)
    if kind == "m":
        return jget(ctx["meta"] or {}, name)
    if kind == "bm":
        return jget(ctx["body_meta"] or {}, name)
    if kind == "expr" and name == "window_number":
        return ctx.get("window_number", WINDOW_NUMBER)
    if kind == "expr" and name == "thread_window":
        # 契约是"<出站 thread>:<入站序号>"，不是某个固定数字。上一版把 ":1" 写死，
        # 只在探针恰好发 1 时成立，反而会保护"序号被改写"的实现。
        t = hdr(ctx["row"], "thread-id")
        return ("%s:%d" % (t, ctx.get("window_number", WINDOW_NUMBER))) if t else None
    raise AssertionError("unknown token " + token)


# ── 时区替换检查（klno codex_request_timezone / codexRequestTimezone）─────────

ENV_CONTEXT_TZ_RE = re.compile(r"<timezone>([^<]*)</timezone>")
ENV_CONTEXT_DATE_RE = re.compile(r"<current_date>([^<]*)</current_date>")


def input_texts(body):
    """按 Responses 的两种文本载体取出 input 里的文本：input 字符串、message 的文本项。"""
    out = []
    if not isinstance(body, dict):
        return out
    raw_input = body.get("input")
    if isinstance(raw_input, str):
        out.append(raw_input)
        return out
    if not isinstance(raw_input, list):
        return out
    for item in raw_input:
        if not isinstance(item, dict):
            continue
        content = item.get("content")
        if isinstance(content, str):
            out.append(content)
        elif isinstance(content, list):
            for part in content:
                if isinstance(part, dict) and isinstance(part.get("text"), str):
                    out.append(part["text"])
    return out


def env_context_values(body):
    """取出出站环境块里的 (timezone, current_date)；没有环境块返回 (None, None)。"""
    for text in input_texts(body):
        if "<environment_context>" not in text:
            continue
        tz = ENV_CONTEXT_TZ_RE.search(text)
        date = ENV_CONTEXT_DATE_RE.search(text)
        return (tz.group(1) if tz else None, date.group(1) if date else None)
    return (None, None)


def expected_dates(timezone):
    """目标时区"今天"的 ISO 日期集合（含前后一天，容忍跨零点与安装时差）。
    tz 数据不可用时返回空集合 → 跳过日期断言。"""
    try:
        from datetime import datetime, timedelta
        from zoneinfo import ZoneInfo
        today = datetime.now(ZoneInfo(timezone)).date()
        return {(today + timedelta(days=d)).isoformat() for d in (-1, 0, 1)}
    except Exception:
        return set()


def timezone_env_problems(body, sentinel, expected, label):
    """环境块时区断言。expected 为空 = 未声明期望值，整体跳过（不判改写与否）。"""
    if not expected:
        return []
    out_tz, out_date = env_context_values(body)
    if out_tz is None:
        return ["%s 出站环境块里没有 <timezone>（应改写为 %s）" % (label, expected)]
    if out_tz == sentinel:
        return ["%s 环境块时区未被改写，仍是探针发的 %s" % (label, sentinel)]
    if out_tz != expected:
        return ["%s 环境块时区 %r != 期望 %r" % (label, out_tz, expected)]
    dates = expected_dates(expected)
    if dates and out_date not in dates:
        return ["%s 环境块日期 %r 与目标时区 %s 的当天不符（只改时区没改日期？）"
                % (label, out_date, expected)]
    return []


def search_location_problems(body, sentinel, expected, label):
    """alpha/search 出站 user_location.timezone 断言：只看存在的那一种形态——
    standalone 是 settings.user_location，PAT 桥接是 tools[].user_location 加 prompt 里的
    settings 副本。expected 为空 = 跳过。"""
    if not expected or not isinstance(body, dict):
        return []
    problems = []
    found = []
    standalone = jget(body, "settings.user_location.timezone")
    if isinstance(standalone, str):
        found.append(("settings.user_location.timezone", standalone))
    tools = body.get("tools")
    if isinstance(tools, list):
        for idx, tool in enumerate(tools):
            if not isinstance(tool, dict):
                continue
            location = tool.get("user_location")
            if isinstance(location, dict) and isinstance(location.get("timezone"), str):
                found.append(("tools[%d].user_location.timezone" % idx, location["timezone"]))
    if not found:
        problems.append("%s 出站请求里找不到 user_location.timezone（探针发过，不该消失）" % label)
    for name, value in found:
        if value == sentinel:
            problems.append("%s %s 未被改写，仍是探针发的 %s" % (label, name, sentinel))
        elif value != expected:
            problems.append("%s %s 为 %r != 期望 %r" % (label, name, value, expected))
    # prompt 里那份 settings 副本（仅 PAT 桥接路径有），必须与工具侧同值。
    for text in input_texts(body):
        if '"user_location"' in text and sentinel in text:
            problems.append("%s prompt 里的 settings 副本仍带探针时区 %s" % (label, sentinel))
    return problems


def check_rows(rows, cases, cross_check=True):
    """rows 与 cases 一一对应；返回 problems 列表。
    cross_check=False 时不做跨路径设备一致性判断，留给调用方合并 WS 结果后统一判。"""
    problems = []
    devices = {}

    if len(rows) != len(cases):
        problems.append("捕获数 %d != 用例数 %d（有用例没发出去或多发了）" % (len(rows), len(cases)))

    for idx, case in enumerate(cases):
        label = case["label"]
        if case.get("curl_rc") not in (None, 0):
            problems.append("%s curl 退出码 %s" % (label, case["curl_rc"]))
        if idx >= len(rows):
            problems.append("%s 没有对应的出站捕获" % label)
            continue
        row = rows[idx]
        path = row.get("path", "?")
        if path != case["upstream"]:
            problems.append("%s 出站路径 %s != 预期 %s" % (label, path, case["upstream"]))
            continue

        spec = CONTRACTS[case["contract"]]
        raw_body = row.get("body")
        body = raw_body if isinstance(raw_body, dict) else {}
        if raw_body and not isinstance(raw_body, dict):
            parsed = parse_meta(raw_body, "请求体", path, problems)
            body = parsed if isinstance(parsed, dict) else {}
        if not body:
            problems.append("%s 没抓到请求体，body 侧断言全部落空" % label)

        meta_hdr = parse_meta(hdr(row, "x-codex-turn-metadata"), "头 turn-metadata", path, problems)
        cm = body.get("client_metadata") or {}
        if not isinstance(cm, dict):
            problems.append("%s client_metadata 必须是对象" % label)
            cm = {}
        meta_body = parse_meta(cm.get("x-codex-turn-metadata"), "体 turn-metadata", path, problems)
        ctx = {"row": row, "body": body, "meta": meta_hdr, "body_meta": meta_body}

        for name in spec["required_headers"]:
            if not (hdr(row, name) or "").strip():
                problems.append("%s 缺必需头 %s" % (label, name))
        for name in spec["forbidden_headers"]:
            if hdr(row, name) is not None:
                problems.append("%s 不该发的头 %s=%r" % (label, name, hdr(row, name)))
        for field in spec["required_body"]:
            value = jget(body, field)
            if not isinstance(value, str) or not value.strip():
                problems.append("%s 缺必需 body 字段 %s" % (label, field))
        for field in spec.get("forbidden_body", []):
            if field in body:
                problems.append("%s 不该有的 body 字段 %s" % (label, field))

        beta = (hdr(row, "openai-beta") or "").lower()
        if FORBIDDEN_BETA in beta:
            problems.append("%s 仍在发旧的 openai-beta %s" % (label, FORBIDDEN_BETA))
        if meta_hdr is not None and "tool_namespaces_info" in meta_hdr:
            problems.append("%s 兼容头 turn-metadata 未剥掉 tool_namespaces_info" % label)
        if meta_hdr is not None and meta_hdr.get("window_number") != WINDOW_NUMBER:
            problems.append("%s turn-metadata 的 window_number 被改写：%r != %r"
                            % (label, meta_hdr.get("window_number"), WINDOW_NUMBER))
        if meta_body is not None and meta_body.get("tool_namespaces_info") != TOOLS:
            problems.append("%s 体内 turn-metadata 的工具清单被误删或改写（只该从头剥）" % label)

        # 设备载体：按端点声明，不做「有一个就行」的兜底
        carriers = {}
        for carrier in spec["device_carrier"]:
            if carrier == "header_install":
                carriers[carrier] = hdr(row, "x-codex-installation-id")
            elif carrier == "body_install":
                carriers[carrier] = cm.get("x-codex-installation-id")
            elif carrier == "meta_install":
                carriers[carrier] = jget(meta_hdr or {}, "installation_id")
            elif carrier == "body_meta_install":
                carriers[carrier] = jget(meta_body or {}, "installation_id")
        for carrier, value in carriers.items():
            if not isinstance(value, str) or not value.strip():
                problems.append("%s 设备载体 %s 缺失" % (label, carrier))
        present = [v for v in carriers.values() if isinstance(v, str) and v.strip()]
        if len(set(present)) > 1:
            problems.append("%s 同一请求内出现多个设备身份 %s" % (label, sorted(set(present))))
        for value in present:
            devices.setdefault(value, []).append(label)

        for left, right in spec["relations"]:
            lv, rv = resolve(left, ctx), resolve(right, ctx)
            if lv is None or rv is None:
                problems.append("%s 关系 %s == %s 无法判定（%s=%r %s=%r）" %
                                (label, left, right, left, lv, right, rv))
            elif lv != rv:
                problems.append("%s 关系不成立 %s(%r) != %s(%r)" % (label, left, lv, right, rv))

        ua = hdr(row, "user-agent") or ""
        if not ua.rstrip().endswith(")"):
            problems.append("%s UA 缺尾部客户端标识组" % label)

        # 时区替换（可选断言）：只有声明了期望时区才判定，否则整体跳过。
        if case.get("expect_env_timezone"):
            problems += timezone_env_problems(body, SENTINEL_TZ, EXPECTED_TZ, label)
        if case.get("expect_search_timezone"):
            problems += search_location_problems(body, SENTINEL_TZ, EXPECTED_TZ, label)

    if cross_check:
        problems += cross_device_problems(devices)
    return problems, devices


def cross_device_problems(devices):
    if len(devices) > 1:
        return ["跨路径出现 %d 个不同设备身份：%s" %
                (len(devices), {k: sorted(set(v)) for k, v in devices.items()})]
    return []


# ── 自检：构造坏样本，验证检查器会报错 ─────────────────────────────────────

def selftest():
    good_meta = {"installation_id": "I", "session_id": "S", "thread_id": "S",
                 "turn_id": "T", "root_turn_id": "T", "window_id": "S:%d" % WINDOW_NUMBER,
                 "context_window_id": "C",
                 "window_number": WINDOW_NUMBER}
    base_headers = [["originator", "codex-tui"], ["user-agent", UA], ["version", "0.153.4"],
                    ["session-id", "S"], ["thread-id", "S"], ["x-client-request-id", "S"],
                    ["x-codex-window-id", "S:%d" % WINDOW_NUMBER],
                    ["x-codex-turn-metadata", json.dumps(good_meta)]]
    base_body = {"prompt_cache_key": "S",
                 "client_metadata": {"session_id": "S", "thread_id": "S",
                                     "x-codex-installation-id": "I",
                                     "x-codex-window-id": "S:%d" % WINDOW_NUMBER,
                                     "x-codex-turn-metadata": json.dumps(
                                         dict(good_meta, tool_namespaces_info=TOOLS))}}
    case = {"label": "T", "upstream": "/backend-api/codex/responses",
            "contract": "responses", "curl_rc": 0}

    def row(headers=None, body=None):
        return {"kind": "http", "path": "/backend-api/codex/responses",
                "headers": [list(h) for h in (headers if headers is not None else base_headers)],
                "body": json.loads(json.dumps(body if body is not None else base_body))}

    ok, _ = check_rows([row()], [case])
    if ok:
        print("[SELFTEST FAIL] 正样本被误报：%s" % ok)
        return 1

    def mutate(fn):
        h = [list(x) for x in base_headers]
        b = json.loads(json.dumps(base_body))
        fn(h, b)
        return row(h, b)

    negatives = [
        ("少发一个用例", [], [case], "捕获数"),
        ("curl 失败", [row()], [dict(case, curl_rc=7)], "curl 退出码"),
        ("路径不对", [dict(row(), path="/wrong")], [case], "出站路径"),
        ("缺必需头 session-id",
         [mutate(lambda h, b: h.remove(next(x for x in h if x[0] == "session-id")))], [case],
         "缺必需头"),
        ("多发独立安装头",
         [mutate(lambda h, b: h.append(["x-codex-installation-id", "I"]))], [case],
         "不该发的头"),
        ("下划线别名", [mutate(lambda h, b: h.append(["session_id", "S"]))], [case], "不该发的头"),
        ("旧 openai-beta",
         [mutate(lambda h, b: h.append(["openai-beta", FORBIDDEN_BETA]))], [case], "openai-beta"),
        ("头 metadata 没剥工具清单",
         [mutate(lambda h, b: h.__setitem__(
             next(i for i, x in enumerate(h) if x[0] == "x-codex-turn-metadata"),
             ["x-codex-turn-metadata", json.dumps(dict(good_meta, tool_namespaces_info=TOOLS))]))],
         [case], "未剥掉 tool_namespaces_info"),
        ("体 metadata 工具清单被误删",
         [mutate(lambda h, b: b["client_metadata"].__setitem__(
             "x-codex-turn-metadata", json.dumps(good_meta)))], [case], "被误删"),
        ("body 缺设备身份",
         [mutate(lambda h, b: b["client_metadata"].pop("x-codex-installation-id"))], [case],
         "设备载体 body_install 缺失"),
        ("同请求两套设备身份",
         [mutate(lambda h, b: b["client_metadata"].__setitem__(
             "x-codex-installation-id", "OTHER"))], [case], "多个设备身份"),
        ("缓存键与会话头不同源",
         [mutate(lambda h, b: b.__setitem__("prompt_cache_key", "OTHER"))], [case], "关系不成立"),
        ("窗口序号被改写",
         [mutate(lambda h, b: h.__setitem__(
             next(i for i, x in enumerate(h) if x[0] == "x-codex-window-id"),
             ["x-codex-window-id", "S:0"]))], [case], "关系不成立"),
        ("metadata 窗口序号被改写",
         [mutate(lambda h, b: h.__setitem__(
             next(i for i, x in enumerate(h) if x[0] == "x-codex-turn-metadata"),
             ["x-codex-turn-metadata", json.dumps(dict(good_meta, window_number=0))]))],
         [case], "window_number 被改写"),
        ("窗口结构被压成裸 UUID",
         [mutate(lambda h, b: h.__setitem__(
             next(i for i, x in enumerate(h) if x[0] == "x-codex-window-id"),
             ["x-codex-window-id", "S"]))], [case], "关系不成立"),
        ("body 整段丢失", [row(body={})], [case], "没抓到请求体"),
        ("UA 缺尾部组",
         [mutate(lambda h, b: h.__setitem__(
             next(i for i, x in enumerate(h) if x[0] == "user-agent"),
             ["user-agent", "codex-tui/0.153.4"]))], [case], "UA 缺尾部"),
    ]

    for field in ("installation_id", "session_id", "thread_id", "window_id",
                  "window_number", "context_window_id", "turn_id", "root_turn_id"):
        def corrupt_body_metadata(h, b, field=field):
            bm = json.loads(b["client_metadata"]["x-codex-turn-metadata"])
            bm[field] = "WRONG"
            b["client_metadata"]["x-codex-turn-metadata"] = json.dumps(bm)
        negatives.append(("体内 metadata." + field + " 错误",
                          [mutate(corrupt_body_metadata)], [case],
                          "多个设备身份" if field == "installation_id" else "关系不成立"))
    negatives.append(("体内 metadata 缺失",
                      [mutate(lambda h, b: b["client_metadata"].pop("x-codex-turn-metadata"))],
                      [case], "缺必需 body"))
    for field in ("context_window_id", "turn_id", "root_turn_id"):
        def drop_body_metadata(h, b, field=field):
            bm = json.loads(b["client_metadata"]["x-codex-turn-metadata"])
            bm.pop(field)
            b["client_metadata"]["x-codex-turn-metadata"] = json.dumps(bm)
        negatives.append(("体内 metadata." + field + " 缺失",
                          [mutate(drop_body_metadata)], [case], "无法判定"))
    failures = 0
    for name, rows, cases, expect in negatives:
        problems, _ = check_rows(rows, cases)
        if not any(expect in p for p in problems):
            print("[SELFTEST FAIL] 反例「%s」没有被 %r 捕获，实得：%s" % (name, expect, problems))
            failures += 1
        else:
            print("  [ok] 反例被拦下：%s" % name)

    # 跨路径设备不一致
    two = [row(), dict(row(), path="/backend-api/codex/responses")]
    two[1]["body"]["client_metadata"]["x-codex-installation-id"] = "J"
    two[1]["body"]["client_metadata"]["x-codex-turn-metadata"] = json.dumps(
        dict(good_meta, installation_id="J", tool_namespaces_info=TOOLS))
    two[1]["headers"] = [list(x) for x in base_headers]
    idx = next(i for i, x in enumerate(two[1]["headers"]) if x[0] == "x-codex-turn-metadata")
    two[1]["headers"][idx] = ["x-codex-turn-metadata", json.dumps(dict(good_meta, installation_id="J"))]
    problems, _ = check_rows(two, [case, dict(case, label="T2")])
    if not any("不同设备身份" in p for p in problems):
        print("[SELFTEST FAIL] 跨路径设备不一致没被拦下：%s" % problems)
        failures += 1
    else:
        print("  [ok] 反例被拦下：跨路径设备不一致")

    failures += selftest_ws()
    failures += selftest_timezone()
    print("\n自检结果：%s" % ("全部反例均被拦下" if failures == 0 else "%d 条未被拦下" % failures))
    return 1 if failures else 0


def selftest_timezone():
    """时区检查器的反例自检：这些样本不联网，纯构造。"""
    failures = 0
    expected = "America/Los_Angeles"
    dates = expected_dates(expected)
    today = sorted(dates)[1] if dates else "2026-06-20"
    # 明显超出 ±1 天容忍窗口的陈旧日期，用来验证"只改时区没改日期"会被拦下。
    try:
        from datetime import datetime, timedelta
        from zoneinfo import ZoneInfo
        stale = (datetime.now(ZoneInfo(expected)).date() - timedelta(days=5)).isoformat()
    except Exception:
        stale = "2000-01-01"

    def env_body(tz, date=today, text_prefix=""):
        text = "%s<environment_context>\n  <current_date>%s</current_date>\n  <timezone>%s</timezone>\n</environment_context>" % (text_prefix, date, tz)
        return {"input": [{"type": "message", "role": "user",
                           "content": [{"type": "input_text", "text": text}]}]}

    def search_body(location, prompt=None):
        body = {"settings": {"user_location": location}}
        if prompt is not None:
            body["input"] = [{"type": "message", "role": "user",
                              "content": [{"type": "input_text", "text": prompt}]}]
        return body

    checks = [
        ("时区正样本被误报", timezone_env_problems(env_body(expected), SENTINEL_TZ, expected, "T"), []),
        ("环境块仍是探针时区",
         timezone_env_problems(env_body(SENTINEL_TZ), SENTINEL_TZ, expected, "T"), "未被改写"),
        ("环境块时区丢失", timezone_env_problems({"input": "hi"}, SENTINEL_TZ, expected, "T"),
         "没有 <timezone>"),
        ("环境块时区被改成别的区",
         timezone_env_problems(env_body("Asia/Shanghai"), SENTINEL_TZ, expected, "T"), "!= 期望"),
        ("声明期望值时未改写不报错",
         timezone_env_problems(env_body(SENTINEL_TZ), SENTINEL_TZ, "", "T"), []),
        ("user_location 正样本被误报",
         search_location_problems(search_body({"timezone": expected}), SENTINEL_TZ, expected, "T"), []),
        ("user_location 仍是探针时区",
         search_location_problems(search_body({"timezone": SENTINEL_TZ}), SENTINEL_TZ, expected, "T"),
         "未被改写"),
        ("user_location 整个消失",
         search_location_problems(search_body({}), SENTINEL_TZ, expected, "T"), "不该消失"),
        ("prompt 副本没跟着改",
         search_location_problems(
             search_body({"timezone": expected},
                         prompt='Search settings JSON:\n{"user_location":{"timezone":"%s"}}' % SENTINEL_TZ),
             SENTINEL_TZ, expected, "T"), "settings 副本"),
    ]
    if dates:
        checks.append(("只改时区没改日期",
                       timezone_env_problems(env_body(expected, date=stale), SENTINEL_TZ, expected, "T"),
                       "只改时区没改日期"))

    for name, problems, expect in checks:
        hit = (not problems) if expect == [] else any(expect in p for p in problems)
        if not hit:
            print("[SELFTEST FAIL] 时区反例「%s」判定错误，实得：%s" % (name, problems))
            failures += 1
        else:
            print("  [ok] 时区样本判定正确：%s" % name)
    return failures


def selftest_ws():
    good_meta = {"installation_id": "I", "session_id": "S", "thread_id": "S",
                 "turn_id": "T", "root_turn_id": "T", "window_id": "S:%d" % WINDOW_NUMBER,
                 "context_window_id": "C",
                 "window_number": WINDOW_NUMBER}
    hs_headers = [["originator", "codex-tui"], ["user-agent", UA], ["version", "0.153.4"],
                  ["session-id", "S"], ["thread-id", "S"], ["x-client-request-id", "S"],
                  ["x-codex-window-id", "S:%d" % WINDOW_NUMBER],
                  ["x-codex-turn-metadata", json.dumps(good_meta)],
                  ["openai-beta", "responses_websockets=2026-02-06"]] + \
                 [[k, v] for k, v in sorted(WS_CONDITIONAL.items())]
    frame_body = {"type": "response.create", "model": "gpt-5.4", "prompt_cache_key": "S",
                  "client_metadata": {"session_id": "S", "thread_id": "S",
                                      "x-codex-installation-id": "I",
                                      "x-codex-window-id": "S:%d" % WINDOW_NUMBER,
                                      "x-codex-turn-metadata": json.dumps(
                                          dict(good_meta, tool_namespaces_info=TOOLS))}}

    def rows(headers=None, frames=2, mutate_frame=None, drop_handshake=False):
        out = []
        if not drop_handshake:
            out.append({"kind": "ws_handshake", "path": WS_PATH,
                        "headers": [list(h) for h in (headers or hs_headers)]})
        for i in range(frames):
            b = json.loads(json.dumps(frame_body))
            number = WINDOW_NUMBER + i
            b["client_metadata"]["x-codex-window-id"] = "S:%d" % number
            b["client_metadata"]["x-codex-turn-metadata"] = json.dumps(
                dict(good_meta, window_id="S:%d" % number, window_number=number,
                     tool_namespaces_info=TOOLS))
            if mutate_frame:
                mutate_frame(i, b)
            out.append({"kind": "ws_message", "opcode": 1, "body": b})
        return out

    def run(rs):
        p = []
        p += cross_device_problems(check_ws(rs, p)) or []
        return p

    ok = run(rows())
    if ok:
        print("[SELFTEST FAIL] WS 正样本被误报：%s" % ok)
        return 1

    def hs_without(name):
        return [list(x) for x in hs_headers if x[0] != name]

    def hs_with(name, value):
        h = hs_without(name)
        h.append([name, value])
        return h

    negatives = [
        ("WS 没有上游握手", rows(drop_handshake=True), "没有捕获到上游握手"),
        ("WS 只有首轮没有第二轮", rows(frames=1), "要求首轮与第二轮各一个"),
        ("WS 握手缺 session-id", rows(headers=hs_without("session-id")), "握手缺必需头"),
        ("WS 握手发了独立安装头",
         rows(headers=hs_with("x-codex-installation-id", "I")), "握手不该发的头"),
        ("WS 握手缺 WS 专用 Beta", rows(headers=hs_without("openai-beta")), "握手缺必需头"),
        ("WS 握手发的是旧 Beta",
         rows(headers=hs_with("openai-beta", FORBIDDEN_BETA)), "旧的 openai-beta"),
        ("WS 握手 Beta 值不对",
         rows(headers=hs_with("openai-beta", "responses=v1")), "不是 WS 协商值"),
        ("WS 握手 metadata 没剥工具清单",
         rows(headers=hs_with("x-codex-turn-metadata",
                              json.dumps(dict(good_meta, tool_namespaces_info=TOOLS)))),
         "握手兼容头未剥掉"),
        ("WS 握手窗口序号被改写",
         rows(headers=hs_with("x-codex-window-id", "S:0")), "握手关系不成立"),
        ("WS 握手 metadata 窗口序号被改写",
         rows(headers=hs_with("x-codex-turn-metadata",
                              json.dumps(dict(good_meta, window_number=0)))),
         "window_number 被改写"),
        ("WS 条件头没转发到上游",
         rows(headers=hs_without("x-openai-memgen-request")),
         "握手缺必需头 x-openai-memgen-request"),
        ("WS 条件头被改写",
         rows(headers=hs_with("x-responsesapi-include-timing-metrics", "false")), "被改写"),
        ("WS 第二轮帧丢了设备身份",
         rows(mutate_frame=lambda i, b: b["client_metadata"].pop("x-codex-installation-id")
              if i == 1 else None),
         "WS 第2轮帧 client_metadata 缺 x-codex-installation-id"),
        ("WS 第二轮帧换了会话",
         rows(mutate_frame=lambda i, b: b["client_metadata"].__setitem__("session_id", "OTHER")
              if i == 1 else None),
         "WS 第2轮帧 session 与握手头不同源"),
        ("WS 帧设备身份与握手不同",
         rows(mutate_frame=lambda i, b: b["client_metadata"].__setitem__(
             "x-codex-installation-id", "J")),
         "不同设备身份"),
        ("WS 帧缓存键与握手会话不同源",
         rows(mutate_frame=lambda i, b: b.__setitem__("prompt_cache_key", "OTHER")),
         "prompt_cache_key != 握手 session-id"),
        ("WS 第二轮缺缓存键",
         rows(mutate_frame=lambda i, b: b.pop("prompt_cache_key") if i == 1 else None),
         "缺必需 body 字段 prompt_cache_key"),
        ("WS 第二轮错线程",
         rows(mutate_frame=lambda i, b: b["client_metadata"].__setitem__("thread_id", "OTHER")
              if i == 1 else None),
         "第2轮帧关系不成立"),
        ("WS 第二轮缺嵌入 metadata",
         rows(mutate_frame=lambda i, b: b["client_metadata"].pop("x-codex-turn-metadata")
              if i == 1 else None),
         "缺必需 body 字段 client_metadata.x-codex-turn-metadata"),
        ("WS 第二轮窗口被握手旧值覆盖",
         rows(mutate_frame=lambda i, b: b["client_metadata"].__setitem__(
             "x-codex-window-id", "S:%d" % WINDOW_NUMBER) if i == 1 else None),
         "第2轮帧关系不成立"),
        ("WS 第二轮嵌入身份错误",
         rows(mutate_frame=lambda i, b: b["client_metadata"].__setitem__(
             "x-codex-turn-metadata", json.dumps(dict(good_meta, session_id="OTHER",
                                                     tool_namespaces_info=TOOLS)))
              if i == 1 else None),
         "第2轮帧关系不成立"),
    ]
    for field in ("context_window_id", "turn_id", "root_turn_id"):
        for remove in (False, True):
            def corrupt_frame_metadata(i, b, field=field, remove=remove):
                if i != 1:
                    return
                bm = json.loads(b["client_metadata"]["x-codex-turn-metadata"])
                if remove:
                    bm.pop(field)
                else:
                    bm[field] = "WRONG"
                    if field == "turn_id":
                        bm["root_turn_id"] = "WRONG"
                b["client_metadata"]["x-codex-turn-metadata"] = json.dumps(bm)
            negatives.append(("WS 第二轮 %s %s" % (field, "缺失" if remove else "错误"),
                              rows(mutate_frame=corrupt_frame_metadata), "第2轮帧关系不成立"))

    failures = 0
    for name, rs, expect in negatives:
        problems = run(rs)
        if not any(expect in p for p in problems):
            print("[SELFTEST FAIL] 反例「%s」没有被 %r 捕获，实得：%s" % (name, expect, problems))
            failures += 1
        else:
            print("  [ok] 反例被拦下：%s" % name)
    return failures


# ── WS 入口：网关的 WS 只能由路由决定（handler 的 ResponsesWebSocket），
# HTTP 请求进不去，所以必须真的连一次。服务器没有 ws 库，这里用裸 socket 实现
# 客户端最小子集：握手 + 掩码文本帧 + 读帧。────────────────────────────────

WS_PATH = "/v1/responses"  # 与 HTTP 用例同前缀；WS 由方法+Upgrade 头区分（routes/gateway.go:229）
# 真实 WS 握手条件性携带这两个头（前者 build_responses_compatibility_headers，
# 后者 build_websocket_headers 直接插入 client.rs:1262）。klno.8 才把它们加进
# WS 入站转发白名单，所以探针必须主动带上并断言原样到达，否则该修复无线上证据。
WS_CONDITIONAL = {"x-openai-memgen-request": "true",
                  "x-responsesapi-include-timing-metrics": "true"}
WS_HANDSHAKE_CONTRACT = {
    # 真实 WS 握手：build_websocket_headers（core/src/client.rs:1235）——
    # 会话三件套 + window + turn-metadata + originator + WS 专用 Beta；不发独立安装头。
    "required_headers": ["originator", "user-agent", "version", "session-id", "thread-id",
                         "x-client-request-id", "x-codex-window-id", "x-codex-turn-metadata",
                         "openai-beta"] + sorted(WS_CONDITIONAL),
    "forbidden_headers": ["x-codex-installation-id", "session_id", "conversation_id"],
    "device_carrier": ["meta_install"],
    "relations": [
        ("h:session-id", "h:thread-id"),
        ("h:thread-id", "h:x-client-request-id"),
        ("h:x-codex-window-id", "expr:thread_window"),
        ("m:session_id", "h:session-id"),
    ],
}

# 窗口属于当前帧，不能固定成握手值。这个受控探针仅递增窗口序号，
# 两帧的 turn/root/context 输入刻意相同，因此这些字段须与握手同源；
# 这不是“一切真实 WS 后续轮次都等于握手”的通用协议断言。
WS_FRAME_CONTRACT = {
    "required_body": ["type", "prompt_cache_key", "client_metadata.session_id",
                      "client_metadata.thread_id", "client_metadata.x-codex-installation-id",
                      "client_metadata.x-codex-window-id", "client_metadata.x-codex-turn-metadata"],
    "relations": [
        ("b:client_metadata.thread_id", "h:thread-id"),
        ("b:client_metadata.x-codex-window-id", "expr:thread_window"),
        ("bm:session_id", "b:client_metadata.session_id"),
        ("bm:thread_id", "b:client_metadata.thread_id"),
        ("bm:installation_id", "b:client_metadata.x-codex-installation-id"),
        ("bm:window_id", "b:client_metadata.x-codex-window-id"),
        ("bm:window_number", "expr:window_number"),
        ("bm:turn_id", "bm:root_turn_id"),
        ("bm:turn_id", "m:turn_id"),
        ("bm:root_turn_id", "m:root_turn_id"),
        ("bm:context_window_id", "m:context_window_id"),
    ],
}


def ws_frame(payload, opcode=1):
    data = payload.encode() if isinstance(payload, str) else payload
    header = bytes([0x80 | opcode])
    mask = os.urandom(4)
    n = len(data)
    if n < 126:
        header += bytes([0x80 | n])
    elif n < 65536:
        header += bytes([0x80 | 126]) + struct.pack("!H", n)
    else:
        header += bytes([0x80 | 127]) + struct.pack("!Q", n)
    masked = bytes(b ^ mask[i % 4] for i, b in enumerate(data))
    return header + mask + masked


def ws_drain(sock, seconds):
    """把网关回推的帧读掉，防止内核缓冲写满导致网关侧阻塞。内容不做解析。"""
    deadline = time.time() + seconds
    sock.settimeout(0.5)
    while time.time() < deadline:
        try:
            if not sock.recv(65536):
                return False
        except socket.timeout:
            continue
        except OSError:
            return False
    return True


def ws_probe(session, problems):
    """连一次网关 WS 入口，同一条连接上发两轮 response.create。失败直接记 problem。"""
    host, port = "127.0.0.1", 18080
    tm = turn_meta(session)
    key = base64.b64encode(os.urandom(16)).decode()
    req = (
        "GET %s HTTP/1.1\r\nHost: %s:%d\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
        "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n"
        "authorization: Bearer %s\r\noriginator: codex-tui\r\nuser-agent: %s\r\nversion: 0.153.4\r\n"
        "session-id: %s\r\nthread-id: %s\r\nx-client-request-id: %s\r\n"
        "x-codex-installation-id: %s\r\nx-codex-window-id: %s:%d\r\nx-codex-turn-metadata: %s\r\n%s\r\n"
        % (WS_PATH, host, port, key, KEY, UA, session, session, session, INSTALL,
           session, WINDOW_NUMBER, tm,
           "".join("%s: %s\r\n" % kv for kv in sorted(WS_CONDITIONAL.items())))
    )
    try:
        sock = socket.create_connection((host, port), timeout=20)
        sock.settimeout(20)
        sock.sendall(req.encode())
        head = b""
        while b"\r\n\r\n" not in head:
            chunk = sock.recv(4096)
            if not chunk:
                break
            head += chunk
        status = head.split(b"\r\n", 1)[0].decode(errors="replace")
        if "101" not in status:
            problems.append("WS 握手未升级：%s" % status[:160])
            sock.close()
            return
        for turn in (1, 2):
            payload = {"type": "response.create", "model": "gpt-5.4", "stream": True,
                       "prompt_cache_key": session,
                       "client_metadata": meta(session, WINDOW_NUMBER + turn - 1),
                       "input": [{"type": "message", "role": "user",
                                  "content": [{"type": "input_text", "text": env_context()}]},
                                 {"type": "message", "role": "user", "content": "hi %d" % turn}]}
            sock.sendall(ws_frame(json.dumps(payload)))
            print("  sent: WS 第%d轮 response.create" % turn, flush=True)
            if not ws_drain(sock, 4):
                if turn == 1:
                    problems.append("WS 连接在第二轮之前就被关闭，无法验证复用同一连接的第二轮")
                break
        try:
            sock.sendall(ws_frame(struct.pack("!H", 1000), 8))
        except OSError:
            pass
        sock.close()
    except Exception as e:
        problems.append("WS 探测异常：%r" % e)


def check_ws(rows, problems):
    handshakes = [r for r in rows if r.get("kind") == "ws_handshake"]
    frames = [r for r in rows if r.get("kind") == "ws_message"]
    if not handshakes:
        problems.append("WS 没有捕获到上游握手（网关没有建立 WS 出站）")
        return {}
    if len(handshakes) != 1:
        problems.append("WS 要求恰好一个上游握手，实得 %d" % len(handshakes))
    if len(frames) != 2:
        problems.append("WS 只捕获到 %d 个上游帧，要求首轮与第二轮各一个" % len(frames))

    devices = {}
    hs = handshakes[0]
    meta_hdr = parse_meta(hdr(hs, "x-codex-turn-metadata"), "握手 turn-metadata", "WS", problems)
    ctx = {"row": hs, "body": {}, "meta": meta_hdr}
    spec = WS_HANDSHAKE_CONTRACT
    for name in spec["required_headers"]:
        if not (hdr(hs, name) or "").strip():
            problems.append("WS 握手缺必需头 %s" % name)
    for name in spec["forbidden_headers"]:
        if hdr(hs, name) is not None:
            problems.append("WS 握手不该发的头 %s=%r" % (name, hdr(hs, name)))
    beta = (hdr(hs, "openai-beta") or "").lower()
    if FORBIDDEN_BETA in beta:
        problems.append("WS 握手仍在发旧的 openai-beta %s" % FORBIDDEN_BETA)
    if "responses_websockets" not in beta:
        problems.append("WS 握手的 openai-beta 不是 WS 协商值：%r" % hdr(hs, "openai-beta"))
    if meta_hdr is not None and "tool_namespaces_info" in meta_hdr:
        problems.append("WS 握手兼容头未剥掉 tool_namespaces_info")
    for name, want in WS_CONDITIONAL.items():
        got = hdr(hs, name)
        if got is not None and got != want:
            problems.append("WS 握手条件头 %s 被改写：%r != %r" % (name, got, want))
    if meta_hdr is not None and meta_hdr.get("window_number") != WINDOW_NUMBER:
        problems.append("WS 握手 turn-metadata 的 window_number 被改写：%r != %r"
                        % (meta_hdr.get("window_number"), WINDOW_NUMBER))
    hs_install = jget(meta_hdr or {}, "installation_id")
    if not hs_install:
        problems.append("WS 握手设备载体 meta_install 缺失")
    else:
        devices.setdefault(hs_install, []).append("WS 握手")
    for left, right in spec["relations"]:
        lv, rv = resolve(left, ctx), resolve(right, ctx)
        if lv is None or rv is None:
            problems.append("WS 握手关系 %s == %s 无法判定（%r / %r）" % (left, right, lv, rv))
        elif lv != rv:
            problems.append("WS 握手关系不成立 %s(%r) != %s(%r)" % (left, lv, right, rv))

    # 每一轮帧的 client_metadata 必须与握手同源——真实客户端一条连接内两者
    # 出自同一份 CodexResponsesMetadata。
    for i, fr in enumerate(frames[:2], start=1):
        raw = fr.get("body")
        fb = raw if isinstance(raw, dict) else (parse_meta(raw, "帧体", "WS", problems) or {})
        cm = fb.get("client_metadata") or {}
        if not isinstance(cm, dict):
            problems.append("WS 第%d轮帧 client_metadata 必须是对象" % i)
            cm = {}
        meta_body = parse_meta(cm.get("x-codex-turn-metadata"), "帧 turn-metadata", "WS", problems)
        frame_ctx = {"row": hs, "body": fb, "meta": meta_hdr, "body_meta": meta_body,
                     "window_number": WINDOW_NUMBER + i - 1}
        for field in WS_FRAME_CONTRACT["required_body"]:
            value = jget(fb, field)
            if not isinstance(value, str) or not value.strip():
                problems.append("WS 第%d轮帧缺必需 body 字段 %s" % (i, field))
        if fb.get("type") != "response.create":
            problems.append("WS 第%d轮帧不是 response.create" % i)
        for left, right in WS_FRAME_CONTRACT["relations"]:
            lv, rv = resolve(left, frame_ctx), resolve(right, frame_ctx)
            if lv is None or rv is None or lv != rv:
                problems.append("WS 第%d轮帧关系不成立 %s(%r) != %s(%r)" % (i, left, lv, right, rv))
        if meta_body is not None and meta_body.get("tool_namespaces_info") != TOOLS:
            problems.append("WS 第%d轮帧体内工具清单被误删或改写" % i)
        # 时区替换在 WS 路径同样生效（逐帧改写，见 applyCodexRequestTimezoneRaw）。
        problems += timezone_env_problems(fb, SENTINEL_TZ, EXPECTED_TZ, "WS 第%d轮帧" % i)
        for field in ("session_id", "thread_id", "x-codex-installation-id"):
            if not cm.get(field):
                problems.append("WS 第%d轮帧 client_metadata 缺 %s" % (i, field))
        if cm.get("session_id") and cm["session_id"] != hdr(hs, "session-id"):
            problems.append("WS 第%d轮帧 session 与握手头不同源（%r != %r）" %
                            (i, cm["session_id"], hdr(hs, "session-id")))
        if isinstance(cm.get("x-codex-installation-id"), str) and cm["x-codex-installation-id"].strip():
            devices.setdefault(cm["x-codex-installation-id"], []).append("WS 第%d轮帧" % i)
        if fb.get("prompt_cache_key") != hdr(hs, "session-id"):
            problems.append("WS 第%d轮帧 prompt_cache_key != 握手 session-id" % i)
    return devices


# ── 发送 ──────────────────────────────────────────────────────────────────

def send(case):
    args = ["curl", "-s", "-o", "/dev/null", "-m", "25", "-X", "POST", GW + case["path"],
            "-H", "authorization: Bearer " + KEY, "-H", "content-type: application/json"]
    for k, v in case["headers"].items():
        args += ["-H", "%s: %s" % (k, v)]
    args += ["-d", json.dumps(case["body"])]
    proc = subprocess.run(args, capture_output=True)
    case["curl_rc"] = proc.returncode
    print("  sent: %-16s curl_rc=%d" % (case["label"], proc.returncode), flush=True)


def warm_up_timezone_probe(wait_seconds=5.0):
    """先发一条带环境块的请求，让网关把"代理出口 IP -> 时区"的异步探测跑完并落缓存。
    这条请求产生的捕获行在 start 之前读取，不参与断言。"""
    if not EXPECTED_TZ:
        return
    probe = {"label": "W 热身", "path": "/v1/responses", "contract": "responses",
             "upstream": "/backend-api/codex/responses",
             "body": resp_body(sid(9)), "headers": codex_headers(sid(9))}
    print("== 时区热身：先让网关完成一次代理时区探测 ==", flush=True)
    send(probe)
    time.sleep(wait_seconds)


def main():
    if not KEY or any(ch in KEY for ch in "\r\n"):
        print("需要通过 SUB2API_REHEARSAL_API_KEY 提供有效的预演凭据", file=sys.stderr)
        return 2
    cases = build_cases()
    warm_up_timezone_probe()
    start = sum(1 for _ in open(CAP))
    problems = []
    if EXPECTED_TZ:
        print("时区断言：期望 %s（探针故意发 %s）" % (EXPECTED_TZ, SENTINEL_TZ), flush=True)
    else:
        print("时区断言：跳过（未设置 SUB2API_REHEARSAL_TIMEZONE）", flush=True)
    print("== 发送 %d 个 HTTP 用例 + 1 条 WS 会话（全部落在 echo server，不出网） ==" % len(cases),
          flush=True)
    for case in cases:
        send(case)
    ws_probe(sid(7), problems)
    time.sleep(2)

    rows = [json.loads(l) for l in open(CAP)][start:]
    http_rows = [r for r in rows if r.get("kind") == "http"]
    ws_rows = [r for r in rows if str(r.get("kind", "")).startswith("ws_")]
    print("\n== 捕获 %d 条 HTTP 出站 + %d 条 WS 事件 ==" % (len(http_rows), len(ws_rows)), flush=True)
    for r in http_rows:
        print("  http %s" % r.get("path"))
    for r in ws_rows:
        print("  %s %s" % (r.get("kind"), r.get("path") or ""))

    http_problems, devices = check_rows(http_rows, cases, cross_check=False)
    problems += http_problems
    for k, v in check_ws(ws_rows, problems).items():
        devices.setdefault(k, []).extend(v)
    problems += cross_device_problems(devices)

    print("\n== 设备一致性（按端点声明的载体取值）==")
    for k, v in devices.items():
        print("  %s <- %s" % (k, sorted(set(v))))

    print("\n== 结论 ==")
    if problems:
        for p in sorted(set(problems)):
            print("  [FAIL] " + p)
        return 1
    print("  探针契约通过（%d HTTP 用例 + WS 两轮；第二轮窗口递增）" % len(cases))
    return 0


if __name__ == "__main__":
    sys.exit(selftest() if "--selftest" in sys.argv else main())

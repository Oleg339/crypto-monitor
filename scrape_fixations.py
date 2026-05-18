"""
Scrape fixation (result) messages from trader#3 topic.
Counts SL hits vs TP closes as reported in the channel itself.
"""

import asyncio
import os
import re
from collections import defaultdict

from pyrogram import Client

API_ID   = int(os.environ.get("TELEGRAM_API_ID",  "0"))
API_HASH = os.environ.get("TELEGRAM_API_HASH", "")
GROUP_NAME = "PifSignal"
TOPIC_THREAD_ID = 8  # trader#3 topic


def parse_fixation(text: str):
    """
    Returns dict with signal_id, result ('profit'|'loss'|'unknown'), pct, or None if not a fixation.
    Fixation messages contain "% Profit" or "% Loss".
    """
    t = text.upper()
    if "GEM SIGNAL" in t:
        return None

    has_profit = bool(re.search(r'\d[\d.,]*\s*%\s*PROFIT', t))
    has_loss   = bool(re.search(r'\d[\d.,]*\s*%\s*LOSS',   t))
    if not (has_profit or has_loss):
        return None

    # signal id
    m = re.search(r'SIGNAL\s*ID\s*[:#]?\s*#?(\d+)', text, re.IGNORECASE)
    signal_id = int(m.group(1)) if m else None

    # coin
    m2 = re.search(r'COIN\s*:\s*(\S+)', text, re.IGNORECASE)
    coin_raw = m2.group(1) if m2 else ""
    coin = re.sub(r'[$/]', '', coin_raw.split()[0]) if coin_raw else ""

    # percentage
    m3 = re.search(r'([\d.,]+)\s*%\s*(PROFIT|LOSS)', text, re.IGNORECASE)
    pct  = float(m3.group(1).replace(',', '.')) if m3 else 0.0
    kind = m3.group(2).upper() if m3 else "unknown"
    if kind == "LOSS":
        pct = -pct

    # did it mention SL?
    hit_sl = bool(re.search(r'STOP.?LOSS', text, re.IGNORECASE))

    # which TPs closed?
    tps_closed = re.findall(r'TP\s*(\d+)', text, re.IGNORECASE)
    tps_closed = [int(x) for x in tps_closed]

    return {
        "signal_id": signal_id,
        "coin":      coin,
        "pct":       pct,
        "kind":      kind,  # PROFIT or LOSS
        "hit_sl":    hit_sl,
        "tps":       tps_closed,
        "text":      text[:120].replace('\n', ' '),
    }


async def main():
    async with Client("session_scraper", api_id=API_ID, api_hash=API_HASH) as app:
        me = await app.get_me()
        print(f"[*] Logged in as {me.first_name} (@{me.username})")

        # Find group
        group_id = None
        async for dialog in app.get_dialogs():
            title = getattr(dialog.chat, "title", "") or ""
            if GROUP_NAME.lower() in title.lower():
                group_id = dialog.chat.id
                print(f"[*] Group: '{title}' id={group_id}")
                break
        if not group_id:
            raise RuntimeError("Group not found")

        print(f"[*] Scanning all messages (topic thread_id={TOPIC_THREAD_ID})…")
        fixations = []
        total = 0

        async for msg in app.get_chat_history(group_id):
            text = msg.text or msg.caption or ""
            total += 1
            if total % 500 == 0:
                print(f"    … {total} scanned, {len(fixations)} fixations so far")

            # filter to trader#3 topic
            msg_top = getattr(msg, "reply_to_top_message_id", None) or msg.reply_to_message_id
            if msg_top != TOPIC_THREAD_ID:
                continue

            if "trader#3" not in text.lower():
                continue

            fx = parse_fixation(text)
            if fx:
                fx["date"] = msg.date.strftime("%Y-%m-%d")
                fixations.append(fx)

        print(f"\n[*] Done. {total} messages scanned, {len(fixations)} fixations found.\n")

        # ── Summary ──────────────────────────────────────────────────────────────
        losses  = [f for f in fixations if f["kind"] == "LOSS"]
        profits = [f for f in fixations if f["kind"] == "PROFIT"]
        sl_explicit = [f for f in losses if f["hit_sl"]]

        print(f"{'='*56}")
        print(f"  CHANNEL FIXATIONS SUMMARY (trader#3)")
        print(f"{'='*56}")
        print(f"  Total fixations   : {len(fixations)}")
        print(f"  Profits (TP close): {len(profits)}")
        print(f"  Losses            : {len(losses)}")
        print(f"    of which SL mentioned : {len(sl_explicit)}")
        print()

        # Per-signal breakdown (latest fixation wins if multiple)
        by_signal = {}
        for f in sorted(fixations, key=lambda x: x["date"]):
            if f["signal_id"]:
                by_signal[f["signal_id"]] = f

        sl_signals    = [sid for sid, f in by_signal.items() if f["kind"] == "LOSS"]
        profit_signals = [sid for sid, f in by_signal.items() if f["kind"] == "PROFIT"]

        print(f"  Unique signals with fixation : {len(by_signal)}")
        print(f"  → final result LOSS  : {len(sl_signals)}  {sorted(sl_signals)}")
        print(f"  → final result PROFIT: {len(profit_signals)}  {sorted(profit_signals)}")
        print()

        print(f"── Loss fixations detail {'─'*32}")
        for f in sorted(losses, key=lambda x: (x["date"], x["signal_id"] or 0)):
            print(f"  #{f['signal_id']:>4}  {f['coin']:<14} {f['pct']:+.2f}%  "
                  f"SL={'yes' if f['hit_sl'] else 'no '}  tps={f['tps']}  | {f['text'][:80]}")

        print()
        print(f"── Profit fixations detail {'─'*31}")
        for f in sorted(profits, key=lambda x: (x["date"], x["signal_id"] or 0)):
            print(f"  #{f['signal_id']:>4}  {f['coin']:<14} {f['pct']:+.2f}%  tps={f['tps']}")


if __name__ == "__main__":
    asyncio.run(main())

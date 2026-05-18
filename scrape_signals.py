"""
Telegram Signal Scraper — PifSignal group, trader#3 topic
Requires: pip install pyrogram tgcrypto
"""

import asyncio
import csv
import os
import re
from datetime import datetime

from pyrogram import Client

API_ID    = int(os.environ.get("TELEGRAM_API_ID",  "0"))
API_HASH  = os.environ.get("TELEGRAM_API_HASH", "")
GROUP_NAME = "PifSignal"   # matched against dialog title
OUTPUT_CSV = "signals_trader3.csv"


def is_regular_signal(text: str) -> bool:
    if not text:
        return False
    t = text.upper()
    if "GEM SIGNAL" in t:
        return False
    if re.search(r'\d+[\.,]?\d*\s*%\s*(PROFIT|LOSS)', t):
        return False
    return (
        bool(re.search(r'SIGNAL ID\s*:\s*#\d+', t))
        and "ENTRY:"    in t
        and "TARGETS:"  in t
        and "STOP LOSS:" in t
    )


def parse_signal(text: str, date: datetime) -> dict:
    def g(pattern):
        m = re.search(pattern, text, re.IGNORECASE)
        return m.group(1).strip() if m else ""

    description = ""
    past_sl = False
    for line in text.splitlines():
        l = line.strip()
        if not l:
            continue
        if "STOP LOSS" in l.upper():
            past_sl = True
            continue
        if past_sl and re.match(r'^[—\-\s]+$', l):
            continue
        if past_sl:
            description = l
            break

    # symbol: "$DOT/USDT (2-5x)" → "DOTUSDT"
    raw_coin = g(r'COIN\s*:\s*(\S+(?:\s+\([^)]+\))?)')
    symbol = re.sub(r'[$/]', '', raw_coin.split()[0])  # remove $, /, keep base+USDT

    # entry: "1.240 - 1.250" → two floats
    entry_str = g(r'ENTRY\s*:\s*([^\n]+)')
    entry_parts = [p.strip() for p in entry_str.split('-') if p.strip()]
    entry_low  = entry_parts[0]  if len(entry_parts) > 0 else ""
    entry_high = entry_parts[-1] if len(entry_parts) > 1 else entry_low

    # targets: "1.300 - 1.350 - ..." → tp1..tp8
    targets_str = g(r'TARGETS\s*:\s*([^\n]+)')
    tp_values = [p.strip() for p in targets_str.split('-') if p.strip()]
    tps = {f"tp{i+1}": (tp_values[i] if i < len(tp_values) else "") for i in range(8)}

    row = {
        "date":       date.strftime("%Y-%m-%d"),
        "time":       date.strftime("%H:%M"),
        "signal_id":  g(r'SIGNAL ID\s*:\s*#(\d+)'),
        "symbol":     symbol,
        "direction":  g(r'Direction\s*:\s*(LONG|SHORT)').upper(),
        "entry_low":  entry_low,
        "entry_high": entry_high,
        "sl":         g(r'STOP LOSS\s*:\s*([^\n]+)'),
        **tps,
        "description": description,
    }
    return row


async def resolve_group(app: Client):
    """Find the group chat object by iterating dialogs."""
    print(f"[*] Looking for '{GROUP_NAME}' in your dialogs…")
    async for dialog in app.get_dialogs():
        title = getattr(dialog.chat, "title", "") or ""
        if GROUP_NAME.lower() in title.lower():
            print(f"[*] Found: '{title}' (id={dialog.chat.id})")
            return dialog.chat.id
    raise RuntimeError(f"Group '{GROUP_NAME}' not found in dialogs. Make sure you're a member.")


async def find_topic_id(app: Client, group_id: int) -> int | None:
    """Scan recent messages to find the thread_id for the trader#3 topic."""
    print("[*] Detecting trader#3 topic thread ID from recent messages…")
    counts: dict[int, int] = {}
    async for msg in app.get_chat_history(group_id, limit=500):
        text = msg.text or msg.caption or ""
        if "trader#3" not in text.lower():
            continue
        top = getattr(msg, "reply_to_top_message_id", None) or msg.reply_to_message_id
        if top:
            counts[top] = counts.get(top, 0) + 1

    if not counts:
        print("[!] Could not detect topic ID — will scan ALL messages and filter by content.")
        return None

    best = max(counts, key=counts.get)
    print(f"[*] trader#3 topic thread_id={best} ({counts[best]} matches)")
    return best


async def main():
    session_exists = os.path.exists("session_scraper.session")
    if session_exists:
        phone = None
        print("[*] Using existing session")
    else:
        phone = os.environ.get("TELEGRAM_PHONE") or input("Phone (+375...): ").strip()

    async with Client(
        "session_scraper",
        api_id=API_ID,
        api_hash=API_HASH,
        phone_number=phone,
    ) as app:
        me = await app.get_me()
        print(f"[*] Logged in as: {me.first_name} (@{me.username})")

        group_id = await resolve_group(app)
        topic_id = await find_topic_id(app, group_id)

        print("[*] Fetching all messages… (this may take a while)")
        signals = []
        total   = 0

        async for msg in app.get_chat_history(group_id):
            text = msg.text or msg.caption or ""
            total += 1
            if total % 500 == 0:
                print(f"    … {total} scanned, {len(signals)} signals so far")

            if topic_id:
                msg_top = getattr(msg, "reply_to_top_message_id", None) or msg.reply_to_message_id
                if msg_top != topic_id:
                    continue

            if "trader#3" not in text.lower():
                continue

            if is_regular_signal(text):
                signals.append(parse_signal(text, msg.date))

        print(f"[*] Done. {total} messages scanned, {len(signals)} signals found.")

        if not signals:
            print("[!] No signals found.")
            return

        signals.sort(key=lambda x: (x["date"], x["time"], int(x["signal_id"] or 0)))

        fieldnames = ["date", "time", "signal_id", "symbol", "direction",
                      "entry_low", "entry_high", "sl",
                      "tp1", "tp2", "tp3", "tp4", "tp5", "tp6", "tp7", "tp8",
                      "description"]
        with open(OUTPUT_CSV, "w", newline="", encoding="utf-8") as f:
            writer = csv.DictWriter(f, fieldnames=fieldnames)
            writer.writeheader()
            writer.writerows(signals)

        print(f"[✓] Saved {len(signals)} signals → {OUTPUT_CSV}")


if __name__ == "__main__":
    asyncio.run(main())

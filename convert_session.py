"""Convert Pyrogram SQLite session → gotd/td JSON session."""
import sqlite3, json, base64

DC_ADDRS = {
    1: "149.154.167.51:443",
    2: "149.154.167.41:443",
    3: "149.154.167.51:443",
    4: "149.154.167.91:443",
    5: "91.108.56.130:443",
}

conn = sqlite3.connect("session_scraper.session")
dc_id, api_id, auth_key, user_id = conn.execute(
    "SELECT dc_id, api_id, auth_key, user_id FROM sessions"
).fetchone()[0:4]
conn.close()

data = {
    "DC":      dc_id,
    "Addr":    DC_ADDRS[dc_id],
    "AuthKey": base64.b64encode(auth_key).decode(),
}

with open("session_userbot.json", "w") as f:
    json.dump(data, f)

print(f"✓ Converted: dc={dc_id}  addr={DC_ADDRS[dc_id]}  key_len={len(auth_key)}  user={user_id}")
print("  → session_userbot.json")

import sqlite3
import datetime
import os
import logging

DATA_DIR = os.environ.get('DATA_DIR', '.')
DB_FILE = os.path.join(DATA_DIR, 'scans.db')

def get_connection():
    conn = sqlite3.connect(DB_FILE)
    conn.row_factory = sqlite3.Row
    return conn

def init_db():
    try:
        conn = get_connection()
        c = conn.cursor()
        c.execute('''
            CREATE TABLE IF NOT EXISTS scans (
                ace_id TEXT PRIMARY KEY,
                success BOOLEAN,
                time REAL,
                info TEXT,
                last_updated TIMESTAMP
            )
        ''')
        
        try:
            c.execute('ALTER TABLE scans ADD COLUMN consecutive_fails INTEGER DEFAULT 0')
        except:
            pass # Column likely exists
            
        conn.commit()
        conn.close()
        logging.info("SQLite Database initialized successfully.")
    except Exception as e:
        logging.error(f"Error initializing SQLite Database: {e}")

def save_scan(ace_id, success, time, info):
    try:
        conn = get_connection()
        c = conn.cursor()
        now = datetime.datetime.now().strftime("%Y-%m-%d %H:%M:%S")
        
        if success:
            c.execute('''
                INSERT INTO scans (ace_id, success, time, info, last_updated, consecutive_fails)
                VALUES (?, ?, ?, ?, ?, 0)
                ON CONFLICT(ace_id) DO UPDATE SET
                    success=excluded.success,
                    time=excluded.time,
                    info=excluded.info,
                    last_updated=excluded.last_updated,
                    consecutive_fails=0
            ''', (ace_id, success, time, info, now))
        else:
            c.execute('''
                INSERT INTO scans (ace_id, success, time, info, last_updated, consecutive_fails)
                VALUES (?, ?, ?, ?, ?, 1)
                ON CONFLICT(ace_id) DO UPDATE SET
                    success=excluded.success,
                    time=excluded.time,
                    info=excluded.info,
                    last_updated=excluded.last_updated,
                    consecutive_fails=consecutive_fails + 1
            ''', (ace_id, success, time, info, now))
            
        conn.commit()
        conn.close()
    except Exception as e:
        logging.error(f"Error saving scan to DB: {e}")

def get_all_scans():
    try:
        conn = get_connection()
        c = conn.cursor()
        c.execute('SELECT * FROM scans')
        rows = c.fetchall()
        conn.close()
        
        # Convert to dictionary for easy lookup in template: { "ace_id": { ... } }
        scans = {}
        for r in rows:
            info_str = r['info'] if r['info'] else ""
            res = "N/A"
            bitrate = "N/A"
            peers = "N/A"
            if info_str:
                parts = [p.strip() for p in info_str.split('|')]
                if len(parts) >= 3:
                    res = parts[0]
                    bitrate = parts[1]
                    peers = parts[2].replace("Peers:", "").strip()
                else:
                    res = info_str
                    
            scans[r['ace_id']] = {
                "success": bool(r['success']),
                "time": r['time'],
                "info": info_str,
                "resolution": res,
                "bitrate": bitrate,
                "peers": peers,
                "last_updated": r['last_updated'],
                "consecutive_fails": r['consecutive_fails'] if 'consecutive_fails' in r.keys() else 0
            }
        return scans
    except Exception as e:
        logging.error(f"Error reading scans from DB: {e}")
        return {}

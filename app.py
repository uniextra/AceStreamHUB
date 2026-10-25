import os
import json
import logging
import requests
import re
import hashlib
import uuid
import datetime
import xml.etree.ElementTree as ET
import time
import threading
import subprocess
import queue
from functools import wraps
from werkzeug.security import generate_password_hash, check_password_hash
from bs4 import BeautifulSoup
from flask import Flask, jsonify, request, Response, render_template, redirect, url_for, stream_with_context, session
from apscheduler.schedulers.background import BackgroundScheduler
from thefuzz import process, fuzz
import db
import translations

app = Flask(__name__)
logging.basicConfig(level=logging.INFO, format='%(asctime)s - %(name)s - %(levelname)s - %(message)s')

@app.after_request
def add_header(response):
    response.headers["Cache-Control"] = "no-cache, no-store, must-revalidate"
    response.headers["Pragma"] = "no-cache"
    response.headers["Expires"] = "0"
    response.headers["Access-Control-Allow-Origin"] = "*"
    response.headers["Access-Control-Allow-Headers"] = "Content-Type,Authorization"
    response.headers["Access-Control-Allow-Methods"] = "GET,PUT,POST,DELETE,OPTIONS"
    return response

@app.context_processor
def inject_translations():
    config = load_config()
    lang = config.get("settings", {}).get("language", "es")
    
    def translate(text):
        return translations.gettext(text, lang)
        
    reset_active = os.environ.get('RESET_PASSWORD', '').lower() in ('true', '1', 'yes')
    return dict(_=translate, current_lang=lang, reset_password_active=reset_active)

DATA_DIR = os.environ.get('DATA_DIR', '.')
CONFIG_FILE = os.path.join(DATA_DIR, 'config.json')

# Thread synchronization locks
config_lock = threading.Lock()
channels_lock = threading.RLock()
active_clients_lock = threading.Lock()
active_streams_lock = threading.Lock()

# Global Data structures
raw_sources_db = {} # source_id -> list of channels
epg_sources_db = {} # source_id -> list of epg_channels
epg_programmes_db = {} # source_id -> list of programmes
epg_channels = [] # global list of all mapped channels
channels_db = [] # final list of unified channels

active_clients_count = 0
active_streams = {} # channel_id -> dict with telemetry and clients map

scheduler = BackgroundScheduler()

def load_config():
    default = {"sources": [], "epg_url": "", "mappings": {}, "settings": {}, "preferred_ids": {}, "last_scans": {}}
    with config_lock:
        if os.path.exists(CONFIG_FILE):
            try:
                with open(CONFIG_FILE, 'r', encoding='utf-8') as f:
                    data = json.load(f)
                    
                    # Migration from old format
                    if "sources" not in data and "source_url" in data and data["source_url"]:
                        data["sources"] = [{
                            "id": str(uuid.uuid4())[:8],
                            "name": "Default Source",
                            "url": data["source_url"],
                            "type": data.get("source_type", "m3u"),
                            "interval_minutes": 60,
                            "last_run": None,
                            "status": "Pending"
                        }]
                    
                    default.update(data)
                    return default
            except Exception as e:
                logging.error(f"Error loading config: {e}")
    return default

def save_config(config):
    with config_lock:
        with open(CONFIG_FILE, 'w', encoding='utf-8') as f:
            json.dump(config, f, indent=4)


def setup_security():
    config = load_config()
    changed = False
    
    if 'secret_key' not in config:
        config['secret_key'] = os.urandom(24).hex()
        changed = True
        
    app.secret_key = bytes.fromhex(config['secret_key'])
    
    reset_active = os.environ.get('RESET_PASSWORD', '').lower() in ('true', '1', 'yes')
    if reset_active and 'admin_password_hash' in config:
        del config['admin_password_hash']
        changed = True
        
    if 'admin_user' in config:
        del config['admin_user']
        changed = True
        
    if changed:
        save_config(config)

setup_security()

@app.before_request
def require_login():
    allowed_endpoints = ['login', 'logout', 'static', 'playlist', 'epg_xml_endpoint', 
                         'hdhr_discover', 'hdhr_lineup_status', 'hdhr_lineup', 
                         'hdhr_lineup_post', 'stream']
    if request.endpoint and request.endpoint not in allowed_endpoints:
        if not session.get('logged_in'):
            if request.path.startswith('/api/'):
                return jsonify({"error": "Unauthorized"}), 401
            return redirect(url_for('login', next=request.url))

def get_stable_id(name):
    return str(int(hashlib.md5(name.encode('utf-8')).hexdigest(), 16) % 9000 + 1000)

def parse_m3u_raw(url):
    logging.info(f"Parsing M3U from {url}")
    try:
        if url.startswith('http'):
            if '.ipns.inbrowser.link' in url:
                cid = url.split('://')[1].split('.ipns')[0]
                path = url.split('.link')[1]
                url = f"https://{cid}.ipns.dweb.link{path}"
                logging.info(f"Resolved IPNS URL to: {url}")
            elif '.ipfs.inbrowser.link' in url:
                cid = url.split('://')[1].split('.ipfs')[0]
                path = url.split('.link')[1]
                url = f"https://{cid}.ipfs.dweb.link{path}"
                logging.info(f"Resolved IPFS URL to: {url}")
                
            headers = {'User-Agent': 'Mozilla/5.0'}
            response = requests.get(url, headers=headers, timeout=10)
            response.raise_for_status()
            content = response.text
        else:
            with open(url, 'r', encoding='utf-8') as f:
                content = f.read()
                
        raw_channels = []
        current_title = None
        current_extinf = None
        
        for line in content.splitlines():
            line = line.strip()
            if not line: continue
            
            if line.startswith('#EXTINF'):
                current_extinf = line
                parts = line.split(',', 1)
                if len(parts) > 1:
                    current_title = parts[1].strip()
                else:
                    current_title = f"Channel {len(raw_channels)+1}"
            elif not line.startswith('#'):
                uri = line
                ace_id = None
                if uri.startswith('acestream://'):
                    ace_id = uri.replace('acestream://', '').strip()
                else:
                    match = re.search(r'([a-fA-F0-9]{40})', uri)
                    if match: ace_id = match.group(1)
                
                if ace_id:
                    title = current_title or f"Channel {len(raw_channels)+1}"
                    raw_channels.append({
                        "title": title,
                        "ace_id": ace_id,
                        "extinf": current_extinf
                    })
                current_title = None
                current_extinf = None
        return raw_channels
    except Exception as e:
        logging.error(f"M3U Parse Error: {e}")
        return []

def scrape_url_raw(url):
    logging.info(f"Scraping from {url}")
    try:
        response = requests.get(url, timeout=10)
        soup = BeautifulSoup(response.text, 'html.parser')
        raw_channels = []
        idx = 1
        for link in soup.find_all('a', href=re.compile(r'^acestream://')):
            ace_id = link['href'].replace('acestream://', '')
            title = link.text.strip() or f"Scraped Channel {idx}"
            raw_channels.append({
                "title": title,
                "ace_id": ace_id,
                "extinf": None
            })
            idx += 1
        return raw_channels
    except Exception as e:
        logging.error(f"Scrape Error: {e}")
        return []

def parse_json_raw(url):
    logging.info(f"Fetching JSON from {url}")
    try:
        res = requests.get(url, timeout=15)
        data = res.json()
        raw_channels = []
        for item in data.get('hashes', []):
            raw_channels.append({
                "title": item.get('title', 'Unknown'),
                "ace_id": item.get('hash', ''),
                "extinf": None,
                "provided_tvg_id": item.get('tvg_id', ''),
                "provided_logo": item.get('logo', '')
            })
        return raw_channels
    except Exception as e:
        logging.error(f"JSON Parse Error: {e}")
        return []

def parse_epg_raw(url):
    logging.info(f"Fetching EPG from {url}")
    try:
        res = requests.get(url, timeout=30)
        root = ET.fromstring(res.content)
        channels = []
        programmes = []
        
        now = datetime.datetime.now(datetime.timezone.utc)
        max_time = now + datetime.timedelta(days=2)
        
        for channel in root.findall('channel'):
            c_id = channel.get('id')
            c_name = ""
            display_name = channel.find('display-name')
            if display_name is not None and display_name.text:
                c_name = display_name.text
            else:
                c_name = c_id
                
            c_logo = ""
            icon = channel.find('icon')
            if icon is not None and icon.get('src'):
                c_logo = icon.get('src')
            
            channels.append({"id": c_id, "name": c_name, "logo": c_logo})
            
        for prog in root.findall('programme'):
            start_str = prog.get('start', '')
            stop_str = prog.get('stop', '')
            
            try:
                # Format: 20240212060000 +0100
                stop_dt = datetime.datetime.strptime(stop_str, '%Y%m%d%H%M%S %z')
                if stop_dt > now and stop_dt < max_time:
                    title_elem = prog.find('title')
                    title = title_elem.text if title_elem is not None else "Unknown"
                    desc_elem = prog.find('desc')
                    desc = desc_elem.text if desc_elem is not None else ""
                    
                    start_dt = datetime.datetime.strptime(start_str, '%Y%m%d%H%M%S %z')
                    
                    programmes.append({
                        "channel": prog.get('channel'),
                        "start_time": start_dt.timestamp(),
                        "stop_time": stop_dt.timestamp(),
                        "title": title,
                        "desc": desc
                    })
            except Exception as pe:
                pass
            
        return channels, programmes
    except Exception as e:
        logging.error(f"Error fetching EPG: {e}")
        return [], []

def fetch_source_job(source_id, force=False):
    config = load_config()
    source = next((s for s in config["sources"] if s["id"] == source_id), None)
    if not source: return
    
    url = source["url"]
    src_type = source["type"]
    interval_minutes = int(source.get("interval_minutes", 60))
    cache_file = os.path.join(DATA_DIR, f"cache_{source_id}.json")
    
    use_cache = False
    if not force and "last_run" in source and os.path.exists(cache_file):
        try:
            last_run = datetime.datetime.strptime(source["last_run"], "%Y-%m-%d %H:%M:%S")
            if (datetime.datetime.now() - last_run).total_seconds() / 60 < interval_minutes:
                use_cache = True
        except:
            pass
            
    raw = None
    raw_ch = None
    raw_prog = None
    
    if use_cache:
        logging.info(f"Loading source {source_id} from cache (not expired)")
        try:
            with open(cache_file, 'r', encoding='utf-8') as f:
                cached_data = json.load(f)
                if src_type == 'epg':
                    raw_ch = cached_data.get('raw_ch', [])
                    raw_prog = cached_data.get('raw_prog', [])
                else:
                    raw = cached_data.get('raw', [])
        except Exception as e:
            logging.error(f"Failed to load cache for {source_id}: {e}")
            use_cache = False
            
    if not use_cache:
        logging.info(f"Fetching source {source_id} from web")
        if src_type == 'epg':
            raw_ch, raw_prog = parse_epg_raw(url)
        else:
            if src_type == 'm3u':
                raw = parse_m3u_raw(url)
            elif src_type == 'json':
                raw = parse_json_raw(url)
            else:
                raw = scrape_url_raw(url)
                
        try:
            with open(cache_file, 'w', encoding='utf-8') as f:
                if src_type == 'epg':
                    json.dump({'raw_ch': raw_ch, 'raw_prog': raw_prog}, f)
                else:
                    json.dump({'raw': raw}, f)
        except Exception as e:
            logging.error(f"Failed to save cache for {source_id}: {e}")

    with channels_lock:
        if src_type == 'epg':
            epg_sources_db[source_id] = raw_ch or []
            epg_programmes_db[source_id] = raw_prog or []
            
            global epg_channels
            global epg_programmes
            
            merged_epg = []
            for k, v in epg_sources_db.items():
                merged_epg.extend(v)
            epg_channels = sorted(merged_epg, key=lambda x: x['name'])
            
            merged_prog = []
            for k, v in epg_programmes_db.items():
                merged_prog.extend(v)
            epg_programmes = merged_prog
            
            logging.info(f"Merged {len(epg_channels)} total EPG channels and {len(epg_programmes)} total programmes")
        else:
            raw_sources_db[source_id] = raw or []
        
    if not use_cache:
        source["last_run"] = datetime.datetime.now().strftime("%Y-%m-%d %H:%M:%S")
        if src_type == 'epg':
            source["status"] = f"OK ({len(raw_ch or [])} channels)" if raw_ch is not None else "Error"
        else:
            source["status"] = f"OK ({len(raw or [])} channels)" if raw is not None else "Error"
        save_config(config)
    
    rebuild_channels_db()

def rebuild_channels_db():
    global channels_db
    with channels_lock:
        config = load_config()
        mappings = config.get("mappings", {})
        blacklist = config.get("blacklist_ids", [])
        
        grouped = {}
        
        # Process raw_sources
        for src_id, items in raw_sources_db.items():
            for item in items:
                ace_id = item['ace_id']
                if ace_id in blacklist:
                    continue
                    
                title = item['title']
                extinf = item.get('extinf')
                provided_tvg_id = item.get('provided_tvg_id')
                provided_logo = item.get('provided_logo')
                
                base_title = title.rstrip('* ').strip()
                
                # Clean specific provider suffixes
                if '-->' in base_title:
                    base_title = base_title.split('-->')[0].strip()
                    
                base_title = re.sub(r'(?i)\s+(1080p|720p|4k|hd|fhd|sd|hevc|h265|x265)\s*$', '', base_title).strip()
                
                if base_title not in grouped:
                    grouped[base_title] = {"ace_ids": [], "extinf": extinf, "title": base_title, "provided_tvg_id": provided_tvg_id, "provided_logo": provided_logo}
                    
                grouped[base_title]["ace_ids"].append(ace_id)
                if not grouped[base_title]["extinf"] and extinf:
                    grouped[base_title]["extinf"] = extinf
                if provided_tvg_id and not grouped[base_title].get("provided_tvg_id"):
                    grouped[base_title]["provided_tvg_id"] = provided_tvg_id
                if provided_logo and not grouped[base_title].get("provided_logo"):
                    grouped[base_title]["provided_logo"] = provided_logo

        # Process manual sources
        manual_sources = config.get("manual_sources", [])
        for item in manual_sources:
            ace_id = item['ace_id']
            if ace_id in blacklist:
                continue
                
            base_title = item['title'].strip()
            if base_title not in grouped:
                grouped[base_title] = {"ace_ids": [], "extinf": None, "title": base_title, "provided_tvg_id": None, "provided_logo": None}
                
            grouped[base_title]["ace_ids"].append(ace_id)
                
        channels = []
        used_chnos = set()
        
        for base_title, data in grouped.items():
            chno = get_stable_id(base_title)
            while chno in used_chnos:
                chno = str(int(chno) + 1)
            used_chnos.add(chno)
            
            epg_id = mappings.get(chno, "")
            if not epg_id and data.get("provided_tvg_id"):
                epg_id = data["provided_tvg_id"]
            
            pref_id = config.get("preferred_ids", {}).get(chno)
            ace_ids = list(set(data["ace_ids"]))
            if pref_id and pref_id in ace_ids:
                ace_ids.remove(pref_id)
                ace_ids.insert(0, pref_id)
                
            channels.append({
                "GuideNumber": chno,
                "GuideName": base_title,
                "URL": f"/auto/v{chno}",
                "ace_ids": ace_ids,
                "extinf": data["extinf"],
                "mapped_epg_id": epg_id,
                "mapped_epg_logo": data.get("provided_logo", ""),
                "preferred_id": pref_id
            })
            
        channels.sort(key=lambda x: x["GuideName"])
        
        # Auto-mapping logic
        def clean_string(s):
            s = s.lower()
            s = re.sub(r'[^a-z0-9]', ' ', s)
            s = re.sub(r'\b(hd|fhd|4k|1080p|720p)\b', '', s)
            return s.strip()
            
        if epg_channels:
            epg_names = {clean_string(e["name"]): e["id"] for e in epg_channels}
            epg_names_list = list(epg_names.keys())
            
            for c in channels:
                if not c["mapped_epg_id"]:
                    c_clean = clean_string(c["GuideName"])
                    if not c_clean:
                        continue
                    # Use fuzzywuzzy to find the best match
                    match, score = process.extractOne(c_clean, epg_names_list, scorer=fuzz.token_set_ratio)
                    # If score is > 85%, we consider it a match
                    if score >= 85:
                        mapped_id = epg_names[match]
                        c["mapped_epg_id"] = mapped_id
                        config["mappings"][c["GuideNumber"]] = mapped_id
                        
                if c["mapped_epg_id"]:
                    if not c.get("mapped_epg_logo"):
                        c["mapped_epg_logo"] = next((e["logo"] for e in epg_channels if e["id"] == c["mapped_epg_id"]), "")
            
            # Generate SequenceNumber based on custom order
            custom_order = config.get("custom_order", [])
            def sort_key(c):
                try:
                    return (0, custom_order.index(c["GuideNumber"]))
                except ValueError:
                    return (1, c["GuideName"])
                    
            channels.sort(key=sort_key)
            
            for i, c in enumerate(channels, 1):
                c["SequenceNumber"] = i
                
        channels_db = channels
        logging.info(f"Rebuilt channels_db: {len(channels)} unique channels.")



def init_scheduler():
    scheduler.remove_all_jobs()
    config = load_config()
    for src in config.get("sources", []):
        interval = int(src.get("interval_minutes", 60))
        scheduler.add_job(
            fetch_source_job, 
            'interval', 
            minutes=interval, 
            args=[src["id"]],
            id=src["id"],
            replace_existing=True
        )
        # trigger immediately and synchronously on boot so DB is populated
        try:
            fetch_source_job(src["id"], force=False)
        except Exception as e:
            logging.error(f"Error fetching source {src['id']} on boot: {e}")
        
    # Start scheduler if not running
    if not scheduler.running:
        scheduler.start()

# --- ROUTES ---

@app.route('/login', methods=['GET', 'POST'])
def login():
    config = load_config()
    lang = config.get("settings", {}).get("language", "es")
    is_setup = 'admin_password_hash' not in config
    
    if request.method == 'POST':
        if is_setup:
            password = request.form.get('password')
            confirm_password = request.form.get('confirm_password')
            
            if not password or password != confirm_password:
                return render_template('login.html', error=translations.gettext("Las contraseñas no coinciden", lang), is_setup=True)
                
            config['admin_password_hash'] = generate_password_hash(password)
            save_config(config)
            session['logged_in'] = True
            return redirect(url_for('dashboard'))
        else:
            password = request.form.get('password')
            if check_password_hash(config.get('admin_password_hash', ''), password):
                session['logged_in'] = True
                return redirect(request.args.get('next') or url_for('dashboard'))
            else:
                return render_template('login.html', error=translations.gettext("Contraseña incorrecta", lang), is_setup=False)
                
    return render_template('login.html', is_setup=is_setup)

@app.route('/logout')
def logout():
    session.clear()
    return redirect(url_for('login'))

@app.route('/')
def index():
    return redirect(url_for('dashboard'))

@app.route('/dashboard')
def dashboard():
    config = load_config()
    with channels_lock:
        total_channels = len(channels_db)
        total_streams = sum(len(c["ace_ids"]) for c in channels_db)
    
    with active_streams_lock:
        total_clients = sum(len(ch.get("clients", {})) for ch in active_streams.values())
        
    with active_clients_lock:
        global active_clients_count
        active_clients_count = total_clients
        
    return render_template('dashboard.html', 
                          channels=total_channels, 
                          sources=len(config.get("sources", [])),
                          streams=total_streams,
                          active_clients=total_clients,
                          settings=config.get("settings", {}))

peers_cache = {} # ace_id -> (timestamp, peer_count)
peers_cache_lock = threading.Lock()

def fetch_channel_peers(ace_id: str) -> int:
    if not ace_id:
        return 0
        
    now = time.time()
    with peers_cache_lock:
        if ace_id in peers_cache:
            cached_time, cached_peers = peers_cache[ace_id]
            if now - cached_time < 2.5:
                return cached_peers
                
    proxy_url = os.environ.get('ACESTREAM_URL', 'http://acexy:8080')
    engine_url = os.environ.get('ENGINE_URL', 'http://acestream-engine:6878')
    
    endpoints = [
        f"{proxy_url}/ace/stat?id={ace_id}",
        f"{engine_url}/ace/stat?id={ace_id}",
        f"{engine_url}/stat/{ace_id}",
        f"{proxy_url}/stat/{ace_id}"
    ]
    
    peers = None
    for url in endpoints:
        try:
            r = requests.get(url, timeout=1.0)
            if r.status_code == 200:
                data = r.json()
                if isinstance(data, dict):
                    if "response" in data and isinstance(data["response"], dict):
                        peers = data["response"].get("peers")
                    elif "peers" in data:
                        peers = data.get("peers")
                if peers is not None:
                    peers = int(peers)
                    break
        except Exception:
            continue
            
    with peers_cache_lock:
        if peers is not None:
            peers_cache[ace_id] = (now, peers)
            return peers
        else:
            old_peers = peers_cache.get(ace_id, (now, 0))[1]
            peers_cache[ace_id] = (now, old_peers)
            return old_peers

@app.route('/api/stats')
def api_stats():
    channels_telemetry = []
    total_clients = 0
    now = time.time()
    
    with active_streams_lock:
        for ch_id, ch_info in list(active_streams.items()):
            clients_list = []
            for cid, c_meta in ch_info.get("clients", {}).items():
                clients_list.append({
                    "id": cid,
                    "ip": c_meta.get("ip", "unknown"),
                    "user_agent": c_meta.get("user_agent", "unknown"),
                    "connected_seconds": int(now - c_meta.get("connected_since", now))
                })
            total_clients += len(clients_list)
            
            ace_id = ch_info.get("ace_id", "")
            peers_count = fetch_channel_peers(ace_id) if ace_id else 0
            
            channels_telemetry.append({
                "channel_id": ch_id,
                "channel_name": ch_info.get("channel_name", f"Channel {ch_id}"),
                "ace_id": ace_id,
                "peers": peers_count,
                "status": ch_info.get("status", "DIRECTO (Pass-Through)"),
                "kbps": ch_info.get("kbps", 0),
                "total_mb": round(ch_info.get("total_bytes", 0) / (1024 * 1024), 1),
                "clients_count": len(clients_list),
                "clients": clients_list,
                "uptime_seconds": int(now - ch_info.get("started_at", now))
            })
            
    with active_clients_lock:
        global active_clients_count
        active_clients_count = total_clients
        
    return jsonify({
        "active_clients": total_clients,
        "total_active_channels": len(channels_telemetry),
        "channels": channels_telemetry
    })

@app.route('/sources', methods=['GET'])
def sources():
    config = load_config()
    return render_template('sources.html', sources=config.get("sources", []))

@app.route('/api/sources', methods=['POST'])
def add_source():
    config = load_config()
    data = request.json
    new_src = {
        "id": str(uuid.uuid4())[:8],
        "name": data.get("name", "New Source"),
        "url": data.get("url", ""),
        "type": data.get("type", "m3u"),
        "interval_minutes": int(data.get("interval_minutes", 60)),
        "last_run": None,
        "status": "Pending"
    }
    if "sources" not in config: config["sources"] = []
    config["sources"].append(new_src)
    save_config(config)
    init_scheduler()
    return jsonify({"status": "success"})

@app.route('/api/sources/<source_id>', methods=['DELETE'])
def delete_source(source_id):
    config = load_config()
    config["sources"] = [s for s in config.get("sources", []) if s["id"] != source_id]
    save_config(config)
    with channels_lock:
        if source_id in raw_sources_db:
            del raw_sources_db[source_id]
            rebuild_channels_db()
    init_scheduler()
    return jsonify({"status": "success"})

@app.route('/api/sources/<source_id>/refresh', methods=['POST'])
def refresh_source_api(source_id):
    fetch_source_job(source_id)
    return jsonify({"status": "success"})

@app.route('/channels')
def channels_view():
    scan_db = db.get_all_scans()
    now = datetime.datetime.now()
    for aid, s in scan_db.items():
        try:
            last_dt = datetime.datetime.strptime(s["last_updated"], "%Y-%m-%d %H:%M:%S")
            s["minutes_ago"] = int((now - last_dt).total_seconds() / 60)
        except (ValueError, TypeError, KeyError):
            s["minutes_ago"] = 0
            
        info = s.get("info", "")
        if info:
            parts = [p.strip() for p in info.split('|')]
            if len(parts) == 3:
                s["resolution"] = parts[0]
                s["bitrate"] = parts[1]
                s["peers"] = parts[2].replace("Peers:", "").strip()
            else:
                s["resolution"] = info
                s["bitrate"] = "N/A"
                s["peers"] = "N/A"
        else:
            s["resolution"] = "N/A"
            s["bitrate"] = "N/A"
            s["peers"] = "N/A"
            
    with channels_lock:
        current_channels = list(channels_db)
        current_epg = list(epg_channels)
    return render_template('channels.html', channels=current_channels, epg_channels=current_epg, scan_db=scan_db)

@app.route('/epg')
def epg_view():
    with channels_lock:
        mapped_channels = [dict(c) for c in channels_db if c.get("mapped_epg_id")]
        current_progs = list(epg_programmes)
    
    # Group programmes by EPG ID
    grouped_progs = {}
    for p in current_progs:
        cid = p.get("channel")
        if cid:
            if cid not in grouped_progs:
                grouped_progs[cid] = []
            grouped_progs[cid].append(p)
        
    for cid in grouped_progs:
        grouped_progs[cid].sort(key=lambda x: x.get("start_time", 0))
        
    # Inject programmes into mapped channels
    for c in mapped_channels:
        epg_id = c["mapped_epg_id"]
        c["programmes"] = grouped_progs.get(epg_id, [])
        
    return render_template('epg.html', channels=mapped_channels)

@app.route('/settings')
def settings_view():
    config = load_config()
    return render_template('settings.html', settings=config.get("settings", {}))

@app.route('/api/settings', methods=['POST'])
def save_settings_api():
    config = load_config()
    data = request.json
    if "settings" not in config:
        config["settings"] = {}
    
    if "scan_on_play_enabled" in data:
        config["settings"]["scan_on_play_enabled"] = data["scan_on_play_enabled"]
    if "placeholder_enabled" in data:
        config["settings"]["placeholder_enabled"] = data["placeholder_enabled"]
    if "scan_on_play_interval_minutes" in data:
        config["settings"]["scan_on_play_interval_minutes"] = data["scan_on_play_interval_minutes"]
    if "language" in data:
        config["settings"]["language"] = data["language"]
    if "auto_discard_enabled" in data:
        config["settings"]["auto_discard_enabled"] = data["auto_discard_enabled"]
    if "stream_timeout" in data:
        config["settings"]["stream_timeout"] = data["stream_timeout"]
    if "engine_live_buffer" in data:
        config["settings"]["engine_live_buffer"] = data["engine_live_buffer"]
    if "engine_disk_cache" in data:
        config["settings"]["engine_disk_cache"] = data["engine_disk_cache"]
        
    # Clean up obsolete settings
    config["settings"].pop("scan_adjacent_enabled", None)
    config["settings"].pop("server_ram_buffer_mb", None)
        
    save_config(config)
    
    # Generate engine_args.txt for acestream-engine container
    try:
        buffer = config["settings"].get("engine_live_buffer", 15)
        cache = config["settings"].get("engine_disk_cache", 0)
        
        args = f"--live-buffer {buffer}"
        if cache > 0:
            args += f" --disk-cache-limit {cache*1073741824}" # convert GB to bytes
            
        with open(os.path.join(DATA_DIR, 'engine_args.txt'), 'w') as f:
            f.write(args)
    except Exception as e:
        logging.error(f"Failed to write engine_args.txt: {e}")
        
    return jsonify({"status": "ok"})

@app.route('/api/reorder_channels', methods=['POST'])
def reorder_channels_api():
    config = load_config()
    data = request.json
    if "custom_order" in data:
        config["custom_order"] = data["custom_order"]
        save_config(config)
        rebuild_channels_db()
    return jsonify({"status": "ok"})

@app.route('/api/mappings', methods=['POST'])
def save_mappings():
    config = load_config()
    data = request.json # { "channel_id": "epg_id" }
    
    if "mappings" not in config:
        config["mappings"] = {}
        
    for ch_id, epg_id in data.items():
        if epg_id:
            config["mappings"][ch_id] = epg_id
        elif ch_id in config["mappings"]:
            del config["mappings"][ch_id]
            
    save_config(config)
    rebuild_channels_db()
    return jsonify({"status": "success"})



@app.route('/api/channels/manual', methods=['POST'])
def add_manual_channel():
    import re
    config = load_config()
    data = request.json
    channel_name = data.get("channel_name")
    ace_id = data.get("ace_id")
    
    if not channel_name or not ace_id:
        return jsonify({"error": "Missing parameters"}), 400
        
    if not re.match(r'^[a-fA-F0-9]{40}$', ace_id):
        return jsonify({"error": "Invalid Ace ID format"}), 400
        
    if "manual_sources" not in config:
        config["manual_sources"] = []
        
    # Check if this exactly exists
    exists = any(s["title"] == channel_name and s["ace_id"] == ace_id for s in config["manual_sources"])
    if not exists:
        config["manual_sources"].append({
            "title": channel_name,
            "ace_id": ace_id,
            "extinf": None
        })
        
    # Remove from blacklist if it was there
    if ace_id in config.get("blacklist_ids", []):
        config["blacklist_ids"].remove(ace_id)
        
    save_config(config)
    rebuild_channels_db()
    return jsonify({"status": "success"})

@app.route('/api/channels/<guide_num>/primary', methods=['POST'])
def set_primary_id(guide_num):
    config = load_config()
    data = request.json
    ace_id = data.get("ace_id")
    
    if "preferred_ids" not in config:
        config["preferred_ids"] = {}
        
    config["preferred_ids"][guide_num] = ace_id
    save_config(config)
    rebuild_channels_db()
    return jsonify({"status": "success"})

@app.route('/api/channels/<guide_num>/id/<ace_id>', methods=['DELETE'])
def delete_source_id(guide_num, ace_id):
    config = load_config()
    
    if "blacklist_ids" not in config:
        config["blacklist_ids"] = []
        
    if ace_id not in config["blacklist_ids"]:
        config["blacklist_ids"].append(ace_id)
        
    # Also if it was preferred, remove it
    if config.get("preferred_ids", {}).get(guide_num) == ace_id:
        del config["preferred_ids"][guide_num]
        
    save_config(config)
    rebuild_channels_db()
    return jsonify({"status": "success"})

@app.route('/playlist.m3u')
def playlist():
    with channels_lock:
        current_channels = list(channels_db)
    lines = ["#EXTM3U"]
    for c in current_channels:
        extinf = c.get("extinf", "")
        
        if not extinf:
            extinf = f'#EXTINF:-1 tvg-chno="{c["SequenceNumber"]}",{c["GuideName"]}'
        else:
            if ',' in extinf:
                extinf = re.sub(r',(?!.*,).*$', f',{c["GuideName"]}', extinf)
            else:
                extinf = f'{extinf},{c["GuideName"]}'
                
        if 'tvg-name="' in extinf:
            extinf = re.sub(r'tvg-name="[^"]*"', f'tvg-name="{c["GuideName"]}"', extinf)
        else:
            extinf = re.sub(r'(#EXTINF:\s*[-\d]+)', rf'\1 tvg-name="{c["GuideName"]}"', extinf, count=1)
            
        if c.get("mapped_epg_id"):
            logo_str = f' tvg-logo="{c.get("mapped_epg_logo", "")}"' if c.get("mapped_epg_logo") else ""
            if 'tvg-id="' in extinf:
                extinf = re.sub(r'tvg-id="[^"]*"', f'tvg-id="{c["mapped_epg_id"]}"', extinf)
            else:
                extinf = re.sub(r'(#EXTINF:\s*[-\d]+)', rf'\1 tvg-id="{c["mapped_epg_id"]}"', extinf, count=1)
                
            if 'tvg-logo="' in extinf and c.get("mapped_epg_logo"):
                extinf = re.sub(r'tvg-logo="[^"]*"', f'tvg-logo="{c["mapped_epg_logo"]}"', extinf)
            elif c.get("mapped_epg_logo"):
                extinf = re.sub(r'(#EXTINF:\s*[-\d]+)', rf'\1{logo_str}', extinf, count=1)
                
        lines.append(extinf)
        lines.append(f"http://{request.host}{c['URL']}")
        
    return Response("\n".join(lines), mimetype='audio/x-mpegurl')

@app.route('/epg.xml')
def epg_xml_endpoint():
    root = ET.Element("tv", {"generator-info-name": "AceStream Hub"})
    
    with channels_lock:
        current_channels = list(channels_db)
        current_programmes = list(epg_programmes)
        
    grouped_progs = {}
    for p in current_programmes:
        cid = p.get("channel")
        if cid:
            if cid not in grouped_progs:
                grouped_progs[cid] = []
            grouped_progs[cid].append(p)
        
    for c in current_channels:
        chno = c["GuideNumber"]
        mapped_id = c.get("mapped_epg_id")
        ch_id = mapped_id if mapped_id else chno
        
        ch_elem = ET.SubElement(root, "channel", {"id": ch_id})
        display_name = ET.SubElement(ch_elem, "display-name")
        display_name.text = c["GuideName"]
        if c.get("mapped_epg_logo"):
            ET.SubElement(ch_elem, "icon", {"src": c["mapped_epg_logo"]})
            
        progs = grouped_progs.get(mapped_id, [])
        if not progs:
            now = datetime.datetime.now(datetime.timezone.utc)
            start_str = now.strftime("%Y%m%d000000 +0000")
            end_str = (now + datetime.timedelta(days=1)).strftime("%Y%m%d000000 +0000")
            prog_elem = ET.SubElement(root, "programme", {
                "start": start_str,
                "stop": end_str,
                "channel": ch_id
            })
            title_elem = ET.SubElement(prog_elem, "title", {"lang": "es"})
            title_elem.text = f"Programación de {c['GuideName']}"
            desc_elem = ET.SubElement(prog_elem, "desc", {"lang": "es"})
            desc_elem.text = "Programación no disponible para este canal."
        else:
            for p in progs:
                start_val = p.get("start_time")
                if isinstance(start_val, (int, float)):
                    s_dt = datetime.datetime.fromtimestamp(start_val, tz=datetime.timezone.utc)
                    s_str = s_dt.strftime("%Y%m%d%H%M%S +0000")
                elif isinstance(start_val, datetime.datetime):
                    s_str = start_val.strftime("%Y%m%d%H%M%S +0000")
                else:
                    s_str = str(start_val)
                    
                stop_val = p.get("stop_time") or p.get("end_time")
                if isinstance(stop_val, (int, float)):
                    e_dt = datetime.datetime.fromtimestamp(stop_val, tz=datetime.timezone.utc)
                    e_str = e_dt.strftime("%Y%m%d%H%M%S +0000")
                elif isinstance(stop_val, datetime.datetime):
                    e_str = stop_val.strftime("%Y%m%d%H%M%S +0000")
                else:
                    e_str = str(stop_val)

                prog_elem = ET.SubElement(root, "programme", {
                    "start": s_str,
                    "stop": e_str,
                    "channel": ch_id
                })
                title_elem = ET.SubElement(prog_elem, "title")
                title_elem.text = p.get("title", "Unknown")
                desc_text = p.get("desc") or p.get("description") or ""
                if desc_text:
                    desc_elem = ET.SubElement(prog_elem, "desc")
                    desc_elem.text = desc_text
                    
    xml_bytes = ET.tostring(root, encoding='utf-8', xml_declaration=True)
    return Response(xml_bytes, mimetype='application/xml')

@app.route('/discover.json')
def hdhr_discover():
    config = load_config()
    if not config.get("settings", {}).get("hdhr_enabled", False):
        return "HDHomeRun emulator is disabled", 404
        
    return jsonify({
        "FriendlyName": "AceStream Hub HDHR",
        "Manufacturer": "Silicondust",
        "ModelNumber": "HDTC-2US",
        "FirmwareName": "hdhomeruntc_atsc",
        "TunerCount": 4,
        "FirmwareVersion": "20150826",
        "DeviceID": "12345678",
        "DeviceAuth": "test1234",
        "BaseURL": f"http://{request.host}",
        "LineupURL": f"http://{request.host}/lineup.json"
    })

@app.route('/lineup_status.json')
def hdhr_lineup_status():
    config = load_config()
    if not config.get("settings", {}).get("hdhr_enabled", False):
        return "HDHomeRun emulator is disabled", 404
    return jsonify({
        "ScanInProgress": 0,
        "ScanPossible": 1,
        "Source": "Cable",
        "SourceList": ["Cable"]
    })

@app.route('/lineup.json')
def hdhr_lineup():
    config = load_config()
    if not config.get("settings", {}).get("hdhr_enabled", False):
        return "HDHomeRun emulator is disabled", 404
        
    with channels_lock:
        current_channels = list(channels_db)
    lineup = []
    for c in current_channels:
        lineup.append({
            "GuideNumber": c["GuideNumber"],
            "GuideName": c["GuideName"],
            "URL": f"http://{request.host}{c['URL']}"
        })
    return jsonify(lineup)

@app.route('/lineup.post', methods=['POST'])
def hdhr_lineup_post():
    config = load_config()
    if not config.get("settings", {}).get("hdhr_enabled", False):
        return "HDHomeRun emulator is disabled", 404
    return ""

@app.route('/auto/v<channel_id>')
def stream(channel_id):
    config = load_config()
    with channels_lock:
        channel = next((c for c in channels_db if c["GuideNumber"] == channel_id), None)
    if not channel:
        return "Channel not found", 404
        
    client_ip = request.headers.get('X-Forwarded-For', request.remote_addr or '127.0.0.1')
    if ',' in client_ip:
        client_ip = client_ip.split(',')[0].strip()
    user_agent = request.headers.get('User-Agent', 'Unknown Player')
    client_id = f"{client_ip}_{uuid.uuid4().hex[:6]}"
    
    settings = config.get("settings", {})
    auto_discard = settings.get("auto_discard_enabled", True)
    timeout = int(settings.get("stream_timeout", 30))
    proxy_url = os.environ.get('ACESTREAM_URL', 'http://acexy:8080')
    
    valid_ace_ids = list(channel.get("ace_ids", []))
    if auto_discard:
        all_scans = db.get_all_scans()
        filtered_ids = [aid for aid in valid_ace_ids if all_scans.get(aid, {}).get("consecutive_fails", 0) < 3]
        if filtered_ids:
            valid_ace_ids = filtered_ids
            
    req = None
    connected_ace_id = None
    for ace_id in valid_ace_ids:
        stream_url = f"{proxy_url}/ace/getstream?id={ace_id}"
        logging.info(f"Connecting channel {channel_id} ({channel.get('GuideName')}) from {stream_url} (timeout={timeout}s)")
        try:
            r = requests.get(stream_url, stream=True, timeout=timeout)
            if r.status_code == 200:
                req = r
                connected_ace_id = ace_id
                break
            else:
                logging.warning(f"Fallback: {stream_url} returned HTTP {r.status_code}")
        except Exception as e:
            logging.error(f"Fallback: failed to connect to {stream_url}: {e}")
            continue
            
    if not req:
        logging.error(f"All sources for channel {channel_id} failed!")
        return "Channel stream unavailable: all sources failed", 503
        
    with active_streams_lock:
        if channel_id not in active_streams:
            active_streams[channel_id] = {
                "channel_id": channel_id,
                "channel_name": channel.get("GuideName", f"Channel {channel_id}"),
                "ace_id": connected_ace_id,
                "started_at": time.time(),
                "total_bytes": 0,
                "kbps": 0,
                "status": "DIRECTO (Pass-Through)",
                "clients": {}
            }
        active_streams[channel_id]["clients"][client_id] = {
            "ip": client_ip,
            "user_agent": user_agent,
            "connected_since": time.time()
        }
        
    def stream_generator():
        global active_clients_count
        with active_clients_lock:
            active_clients_count += 1
            
        speed_bytes = 0
        speed_start = time.time()
        chunk_size = 128 * 1024
        
        try:
            for chunk in req.iter_content(chunk_size=chunk_size):
                if not chunk:
                    continue
                now = time.time()
                chunk_len = len(chunk)
                speed_bytes += chunk_len
                
                if now - speed_start >= 1.0:
                    current_kbps = int((speed_bytes * 8) / (1024 * (now - speed_start)))
                    speed_bytes = 0
                    speed_start = now
                    with active_streams_lock:
                        if channel_id in active_streams:
                            active_streams[channel_id]["kbps"] = current_kbps
                            
                with active_streams_lock:
                    if channel_id in active_streams:
                        active_streams[channel_id]["total_bytes"] += chunk_len
                        
                yield chunk
        finally:
            with active_streams_lock:
                if channel_id in active_streams:
                    if client_id in active_streams[channel_id]["clients"]:
                        del active_streams[channel_id]["clients"][client_id]
                    if not active_streams[channel_id]["clients"]:
                        del active_streams[channel_id]
                        
            with active_clients_lock:
                active_clients_count = max(0, active_clients_count - 1)
                
            try:
                req.close()
            except Exception:
                pass
                
    return Response(stream_with_context(stream_generator()), mimetype="video/mp2t")

def perform_channel_scan(chno, limit=None):
    with active_streams_lock:
        if len(active_streams) > 0 and str(chno) not in active_streams:
            logging.info(f"Skipping channel scan for {chno}: active playback is in progress on channel(s) {list(active_streams.keys())}")
            return {"error": "Skipped due to active playback"}
            
    with channels_lock:
        channel = next((c for c in channels_db if c["GuideNumber"] == chno), None)
    if not channel:
        return {"error": "Channel not found"}
        
    ace_ids = channel["ace_ids"]
    if not ace_ids:
        return {"error": "No IDs found"}
        
    if limit is not None and limit > 0:
        ace_ids = ace_ids[:limit]
        
    best_id = None
    best_score = -float('inf')
    results = []
    
    for aid in ace_ids:
        start = time.time()
        success = False
        info = ""
        res_label = "SD"
        peers = 0
        speed_kbps = 0
        
        try:
            engine_url = os.environ.get('ENGINE_URL', 'http://acestream-engine:6878')
            getstream_url = f"{engine_url}/ace/getstream?id={aid}&format=json"
            r = requests.get(getstream_url, timeout=10)
            if r.status_code == 200:
                data = r.json()
                if "response" in data and data["response"]:
                    stat_url = data["response"].get("stat_url")
                    playback_url = data["response"].get("playback_url")
                    
                    if stat_url and playback_url:
                        time.sleep(2.5)
                        
                        stat_r = requests.get(stat_url, timeout=5)
                        if stat_r.status_code == 200:
                            stat_data = stat_r.json()
                            if "response" in stat_data and stat_data["response"]:
                                peers = stat_data["response"].get("peers", 0)
                                speed_kbps = stat_data["response"].get("speed_down", 0)
                        
                        br_mbps = (speed_kbps * 8) / 1000
                        br_label = f"{br_mbps:.1f} Mbps"
                        
                        proxy_url = os.environ.get('ACESTREAM_URL', 'http://acexy:8080')
                        playback_url = f"{proxy_url}/ace/getstream?id={aid}"
                        
                        cmd = [
                            'ffprobe',
                            '-v', 'quiet',
                            '-print_format', 'json',
                            '-show_streams',
                            playback_url
                        ]
                        res = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10, text=True)
                        if res.returncode == 0:
                            probe_data = json.loads(res.stdout)
                            streams = probe_data.get('streams', [])
                            video_stream = next((s for s in streams if s.get('codec_type') == 'video'), None)
                            if video_stream:
                                success = True
                                height = video_stream.get('height', 0)
                                if height >= 1080:
                                    res_label = "FHD"
                                elif height >= 720:
                                    res_label = "HD"
                                    
                        if not success and peers > 0:
                            success = True
                            
                        info = f"{res_label} | {br_label} | Peers: {peers}"
        except Exception as e:
            logging.error(f"Test error for {aid}: {e}")
            
        elapsed = time.time() - start
        
        score = -float('inf')
        if success:
            score = 0
            if res_label == "FHD":
                score += 1000
            elif res_label == "HD":
                score += 500
            score += peers
            score -= (elapsed * 5)
            
        results.append({"id": aid, "success": success, "time": elapsed, "info": info})
        if success and score > best_score:
            best_score = score
            best_id = aid
            
    if best_id:
        config = load_config()
        if "preferred_ids" not in config:
            config["preferred_ids"] = {}
        config["preferred_ids"][chno] = best_id
        
        if "last_scans" not in config:
            config["last_scans"] = {}
        config["last_scans"][chno] = datetime.datetime.now().strftime("%Y-%m-%d %H:%M:%S")
            
        save_config(config)
        
        # Guardar en SQLite
        for res in results:
            db.save_scan(res["id"], res["success"], res["time"], res["info"])
            
        rebuild_channels_db()
        return {"success": True, "best_id": best_id, "results": results}
    else:
        return {"success": False, "results": results, "error": "No working sources found"}

@app.route('/api/test_channel/<chno>', methods=['POST'])
def test_channel(chno):
    result = perform_channel_scan(chno, limit=None)
    if "error" in result and not result.get("results"):
        return jsonify(result), 404
    return jsonify(result)

# Initialize database and scheduler on application load (supports Gunicorn and direct python execution)
db.init_db()
init_scheduler()

if __name__ == '__main__':
    web_port = int(os.environ.get('WEB_PORT', 5004))
    app.run(host='0.0.0.0', port=web_port)


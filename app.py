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
def add_header(r):
    r.headers["Cache-Control"] = "no-cache, no-store, must-revalidate"
    r.headers["Pragma"] = "no-cache"
    r.headers["Expires"] = "0"
    r.headers['Cache-Control'] = 'public, max-age=0'
    response_headers = r
    response_headers.headers.add('Access-Control-Allow-Origin', '*')
    response_headers.headers.add('Access-Control-Allow-Headers', 'Content-Type,Authorization')
    response_headers.headers.add('Access-Control-Allow-Methods', 'GET,PUT,POST,DELETE,OPTIONS')
    response_headers.headers.add('Access-Control-Allow-Methods', 'GET,PUT,POST,DELETE,OPTIONS')
    return response_headers

@app.context_processor
def inject_translations():
    config = load_config()
    lang = config.get("settings", {}).get("language", "es")
    
    def translate(text):
        return translations.gettext(text, lang)
        
    return dict(_=translate, current_lang=lang)

DATA_DIR = os.environ.get('DATA_DIR', '.')
CONFIG_FILE = os.path.join(DATA_DIR, 'config.json')

# Global Data structures
raw_sources_db = {} # source_id -> list of channels
epg_sources_db = {} # source_id -> list of epg_channels
epg_programmes_db = {} # source_id -> list of programmes
epg_channels = [] # global list of all mapped channels
channels_db = [] # final list of unified channels

active_clients_count = 0
active_clients_lock = threading.Lock()

scheduler = BackgroundScheduler()

def load_config():
    default = {"sources": [], "epg_url": "", "mappings": {}, "settings": {}, "preferred_ids": {}, "last_scans": {}}
    if os.path.exists(CONFIG_FILE):
        try:
            with open(CONFIG_FILE, 'r') as f:
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
    with open(CONFIG_FILE, 'w') as f:
        json.dump(config, f, indent=4)

def setup_security():
    config = load_config()
    changed = False
    
    if 'secret_key' not in config:
        config['secret_key'] = os.urandom(24).hex()
        changed = True
        
    app.secret_key = bytes.fromhex(config['secret_key'])
    
    env_user = os.environ.get('ADMIN_USER')
    env_pass = os.environ.get('ADMIN_PASSWORD')
    if env_user and env_pass:
        config['admin_user'] = env_user
        config['admin_password_hash'] = generate_password_hash(env_pass)
        changed = True
    elif 'admin_user' not in config or 'admin_password_hash' not in config:
        config['admin_user'] = 'admin'
        config['admin_password_hash'] = generate_password_hash('admin')
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
    is_default = check_password_hash(config.get('admin_password_hash', ''), 'admin') and config.get('admin_user') == 'admin'
    
    if request.method == 'POST':
        username = request.form.get('username')
        password = request.form.get('password')
        
        if username == config.get('admin_user') and check_password_hash(config.get('admin_password_hash', ''), password):
            session['logged_in'] = True
            return redirect(request.args.get('next') or url_for('dashboard'))
        else:
            return render_template('login.html', error=translations.gettext("Credenciales incorrectas", lang), is_default=is_default)
            
    return render_template('login.html', is_default=is_default)

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
    total_streams = sum(len(c["ace_ids"]) for c in channels_db)
    
    with active_clients_lock:
        active_clients = active_clients_count
        
    return render_template('dashboard.html', 
                          channels=len(channels_db), 
                          sources=len(config.get("sources", [])),
                          streams=total_streams,
                          active_clients=active_clients,
                          settings=config.get("settings", {}))

@app.route('/api/stats')
def api_stats():
    with active_clients_lock:
        active_clients = active_clients_count
    return jsonify({"active_clients": active_clients})

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
        except:
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
            
    return render_template('channels.html', channels=channels_db, epg_channels=epg_channels, scan_db=scan_db)

@app.route('/epg')
def epg_view():
    # Only show channels that are mapped
    mapped_channels = [c for c in channels_db if c.get("mapped_epg_id")]
    
    # Group programmes by EPG ID
    grouped_progs = {}
    for p in epg_programmes:
        cid = p["channel"]
        if cid not in grouped_progs:
            grouped_progs[cid] = []
        grouped_progs[cid].append(p)
        
    for cid in grouped_progs:
        grouped_progs[cid].sort(key=lambda x: x["start_time"])
        
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
    if "scan_adjacent_enabled" in data:
        config["settings"]["scan_adjacent_enabled"] = data["scan_adjacent_enabled"]
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
        
    save_config(config)
    
    # Generate engine_args.txt for acestream-engine container
    try:
        buffer = config["settings"].get("engine_live_buffer", 10)
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
    lines = ["#EXTM3U"]
    for c in channels_db:
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
    import xml.etree.ElementTree as ET
    from xml.dom import minidom
    root = ET.Element("tv", {"generator-info-name": "AceStream Hub"})
    
    grouped_progs = {}
    for p in epg_programmes:
        cid = p["channel"]
        if cid not in grouped_progs:
            grouped_progs[cid] = []
        grouped_progs[cid].append(p)
        
    for c in channels_db:
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
            now = datetime.datetime.now()
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
                prog_elem = ET.SubElement(root, "programme", {
                    "start": p["start_time"].strftime("%Y%m%d%H%M%S +0000"),
                    "stop": p["end_time"].strftime("%Y%m%d%H%M%S +0000"),
                    "channel": ch_id
                })
                title_elem = ET.SubElement(prog_elem, "title")
                title_elem.text = p["title"]
                if p["description"]:
                    desc_elem = ET.SubElement(prog_elem, "desc")
                    desc_elem.text = p["description"]
                    
    xmlstr = minidom.parseString(ET.tostring(root, encoding='utf-8')).toprettyxml(indent="  ")
    return Response(xmlstr, mimetype='application/xml')

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
        
    lineup = []
    for c in channels_db:
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

def scan_adjacent_background(current_chno, scan_interval):
    try:
        config = load_config()
        current_idx = None
        for i, c in enumerate(channels_db):
            if c["GuideNumber"] == current_chno:
                current_idx = i
                break
        if current_idx is None:
            return
            
        adjacents = []
        if current_idx > 0:
            adjacents.append(channels_db[current_idx - 1])
        if current_idx < len(channels_db) - 1:
            adjacents.append(channels_db[current_idx + 1])
            
        for adj in adjacents:
            adj_chno = adj["GuideNumber"]
            last_scans = config.get("last_scans", {})
            last_scan_str = last_scans.get(adj_chno)
            needs_scan = True
            
            if last_scan_str:
                try:
                    last_scan_time = datetime.datetime.strptime(last_scan_str, "%Y-%m-%d %H:%M:%S")
                    if (datetime.datetime.now() - last_scan_time).total_seconds() / 60 < scan_interval:
                        all_scans = db.get_all_scans()
                        has_data = any(aid in all_scans for aid in adj["ace_ids"])
                        if has_data:
                            needs_scan = False
                except:
                    pass
            
            if needs_scan:
                logging.info(f"Background scanning adjacent channel {adj_chno} ({adj['GuideName']})")
                perform_channel_scan(adj_chno, limit=3)
    except Exception as e:
        logging.error(f"Error in background adjacent scan: {e}")

@app.route('/auto/v<channel_id>')
def stream(channel_id):
    import subprocess
    import threading
    config = load_config()
    channel = next((c for c in channels_db if c["GuideNumber"] == channel_id), None)
    if not channel:
        return "Channel not found", 404
        
    settings = config.get("settings", {})
    scan_enabled = settings.get("scan_on_play_enabled", False)
    scan_interval = settings.get("scan_on_play_interval_minutes", 60)
    placeholder_enabled = settings.get("placeholder_enabled", True)
    
    needs_scan = False
    if scan_enabled:
        last_scans = config.get("last_scans", {})
        last_scan_str = last_scans.get(channel_id)
        needs_scan = True
        if last_scan_str:
            try:
                last_scan_time = datetime.datetime.strptime(last_scan_str, "%Y-%m-%d %H:%M:%S")
                if (datetime.datetime.now() - last_scan_time).total_seconds() / 60 < scan_interval:
                    all_scans = db.get_all_scans()
                    has_data = any(aid in all_scans for aid in channel["ace_ids"])
                    if has_data:
                        needs_scan = False
            except:
                pass
                
    if settings.get("scan_adjacent_enabled", False):
        scheduler.add_job(scan_adjacent_background, args=[channel_id, scan_interval])
        
    def stream_generator():
        global active_clients_count
        with active_clients_lock:
            active_clients_count += 1
            
        connect_results = {}
        def background_task():
            if scan_enabled and needs_scan:
                logging.info(f"Scan on Play triggered for channel {channel_id}")
                perform_channel_scan(channel_id, limit=3)
                
            c = next((ch for ch in channels_db if ch["GuideNumber"] == channel_id), channel)
            
            auto_discard = settings.get("auto_discard_enabled", True)
            timeout = int(settings.get("stream_timeout", 30))
            
            valid_ace_ids = c["ace_ids"]
            if auto_discard:
                all_scans = db.get_all_scans()
                filtered_ids = [aid for aid in valid_ace_ids if all_scans.get(aid, {}).get("consecutive_fails", 0) < 3]
                if filtered_ids:
                    valid_ace_ids = filtered_ids
                    
            for ace_id in valid_ace_ids:
                stream_url = f"http://acexy:8080/ace/getstream?id={ace_id}"
                logging.info(f"Attempting to proxy channel {channel_id} from {stream_url} with timeout {timeout}s")
                try:
                    req = requests.get(stream_url, stream=True, timeout=timeout)
                    if req.status_code == 200:
                        connect_results['req'] = req
                        return
                    else:
                        logging.warning(f"Fallback: {stream_url} returned {req.status_code}")
                except Exception as e:
                    logging.error(f"Fallback: failed to connect to {stream_url}: {e}")
                    continue
            connect_results['req'] = None

        bg_thread = threading.Thread(target=background_task)
        bg_thread.start()
        
        config = load_config()
        lang = config.get("settings", {}).get("language", "es")
        text_loading = translations.gettext("Cargando...", lang)
        
        try:
            if placeholder_enabled:
                cmd = [
                    'ffmpeg', '-f', 'lavfi', '-re', '-i', 'color=c=#1b1a2f:s=1280x720:r=25',
                    '-ignore_loop', '0', '-i', 'assets/loading.gif',
                    '-filter_complex', f"[1:v]scale=250:-1[spinner];[0:v][spinner]overlay=(main_w-overlay_w)/2:(main_h-overlay_h)/2+80[bg];[bg]drawtext=text='AceStream Hub - {text_loading}':fontcolor=white:fontsize=48:x=(w-text_w)/2:y=(h-text_h)/2-80",
                    '-c:v', 'libx264', '-preset', 'ultrafast', '-tune', 'zerolatency', '-b:v', '500k', '-f', 'mpegts', 'pipe:1'
                ]
                ffmpeg_proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
                
                while bg_thread.is_alive():
                    chunk = ffmpeg_proc.stdout.read(65536)
                    if chunk:
                        yield chunk
                    else:
                        break
                try:
                    ffmpeg_proc.terminate()
                    ffmpeg_proc.wait(timeout=1)
                except:
                    pass
            else:
                bg_thread.join()
                
            req = connect_results.get('req')
            if req:
                yield from req.iter_content(chunk_size=1024 * 1024)
            else:
                logging.error(f"All sources for channel {channel_id} failed!")
                text_error = translations.gettext("ERROR", lang)
                text_failed = translations.gettext("Todos los origenes fallaron", lang)
                if placeholder_enabled:
                    cmd = [
                        'ffmpeg', '-f', 'lavfi', '-re', '-i', 'color=c=#1b0000:s=1280x720:d=10:r=25',
                        '-vf', f"drawtext=text='AceStream Hub - {text_error}':fontcolor=#ff6b6b:fontsize=48:x=(w-text_w)/2:y=(h-text_h)/2,drawtext=text='{text_failed}':fontcolor=white:fontsize=32:x=(w-text_w)/2:y=(h-text_h)/2+80",
                        '-c:v', 'libx264', '-preset', 'ultrafast', '-tune', 'zerolatency', '-b:v', '500k', '-f', 'mpegts', 'pipe:1'
                    ]
                    ffmpeg_proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
                    while True:
                        chunk = ffmpeg_proc.stdout.read(65536)
                        if chunk:
                            yield chunk
                        else:
                            break
                    ffmpeg_proc.terminate()
        finally:
            with active_clients_lock:
                active_clients_count -= 1
            req = connect_results.get('req')
            if req:
                req.close()
            
    return Response(stream_with_context(stream_generator()), mimetype="video/MP2T")
def perform_channel_scan(chno, limit=None):
    global channels_db
    import subprocess
    import json
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
            engine_url = "http://acestream-engine:6878"
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

if __name__ == '__main__':
    db.init_db()
    init_scheduler()
    web_port = int(os.environ.get('WEB_PORT', 5004))
    app.run(host='0.0.0.0', port=web_port)

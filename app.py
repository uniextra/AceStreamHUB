import os
import json
import logging
import requests
import re
import hashlib
import uuid
import datetime
import xml.etree.ElementTree as ET
from bs4 import BeautifulSoup
from flask import Flask, jsonify, request, Response, render_template, redirect, url_for, stream_with_context
from apscheduler.schedulers.background import BackgroundScheduler
from thefuzz import process, fuzz

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
    return response_headers

CONFIG_FILE = 'config.json'

# Global Data structures
raw_sources_db = {} # source_id -> list of channels
epg_sources_db = {} # source_id -> list of epg_channels
channels_db = []
epg_channels = [] 
scheduler = BackgroundScheduler()

def load_config():
    default = {"sources": [], "epg_url": "", "mappings": {}}
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
            
        return channels
    except Exception as e:
        logging.error(f"Error fetching EPG: {e}")
        return []

def fetch_source_job(source_id):
    config = load_config()
    source = next((s for s in config["sources"] if s["id"] == source_id), None)
    if not source: return
    
    url = source["url"]
    src_type = source["type"]
    
    if src_type == 'epg':
        raw = parse_epg_raw(url)
        epg_sources_db[source_id] = raw
        
        # Merge all epg sources
        global epg_channels
        merged_epg = []
        for k, v in epg_sources_db.items():
            merged_epg.extend(v)
        epg_channels = sorted(merged_epg, key=lambda x: x['name'])
        logging.info(f"Merged {len(epg_channels)} total EPG channels")
    else:
        if src_type == 'm3u':
            raw = parse_m3u_raw(url)
        elif src_type == 'json':
            raw = parse_json_raw(url)
        else:
            raw = scrape_url_raw(url)
            
        raw_sources_db[source_id] = raw
    
    # Update status
    source["last_run"] = datetime.datetime.now().strftime("%Y-%m-%d %H:%M:%S")
    source["status"] = f"OK ({len(raw)} channels)" if raw else "Error"
    save_config(config)
    
    rebuild_channels_db()

def rebuild_channels_db():
    global channels_db
    config = load_config()
    mappings = config.get("mappings", {})
    
    grouped = {}
    for src_id, items in raw_sources_db.items():
        for item in items:
            title = item['title']
            ace_id = item['ace_id']
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
                grouped[base_title]["extinf"] = extinf # keep best extinf
            if provided_tvg_id and not grouped[base_title].get("provided_tvg_id"):
                grouped[base_title]["provided_tvg_id"] = provided_tvg_id
            if provided_logo and not grouped[base_title].get("provided_logo"):
                grouped[base_title]["provided_logo"] = provided_logo
            
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
        
        channels.append({
            "GuideNumber": chno,
            "GuideName": base_title,
            "URL": f"/auto/v{chno}",
            "ace_ids": list(set(data["ace_ids"])),
            "extinf": data["extinf"],
            "mapped_epg_id": epg_id,
            "mapped_epg_logo": data.get("provided_logo", "")
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
                    
    save_config(config)
    
    channels_db = channels
    logging.info(f"Rebuilt channels_db: {len(channels_db)} unique channels.")



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
        # trigger immediately in background
        scheduler.add_job(fetch_source_job, args=[src["id"]], replace_existing=False)
        
    # Start scheduler if not running
    if not scheduler.running:
        scheduler.start()

# --- ROUTES ---

@app.route('/')
def index():
    return redirect(url_for('dashboard'))

@app.route('/dashboard')
def dashboard():
    config = load_config()
    total_streams = sum(len(c["ace_ids"]) for c in channels_db)
    
    active_clients = 0
    try:
        res = requests.get("http://acexy:8080/ace/status", timeout=1)
        if res.status_code == 200:
            active_clients = res.json().get('clients', 0)
    except:
        pass
        
    return render_template('dashboard.html', 
                          channels=len(channels_db), 
                          sources=len(config.get("sources", [])),
                          streams=total_streams,
                          active_clients=active_clients)

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
    return render_template('channels.html', channels=channels_db, epg_channels=epg_channels)

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



@app.route('/playlist.m3u')
def playlist():
    lines = ["#EXTM3U"]
    for c in channels_db:
        extinf = c.get("extinf", "")
        
        if c.get("mapped_epg_id"):
            logo_str = f' tvg-logo="{c.get("mapped_epg_logo", "")}"' if c.get("mapped_epg_logo") else ""
            if extinf:
                if 'tvg-id="' in extinf:
                    extinf = re.sub(r'tvg-id="[^"]*"', f'tvg-id="{c["mapped_epg_id"]}"', extinf)
                else:
                    extinf = extinf.replace('#EXTINF:-1', f'#EXTINF:-1 tvg-id="{c["mapped_epg_id"]}"')
                
                if 'tvg-logo="' in extinf and c.get("mapped_epg_logo"):
                    extinf = re.sub(r'tvg-logo="[^"]*"', f'tvg-logo="{c["mapped_epg_logo"]}"', extinf)
                elif c.get("mapped_epg_logo"):
                    extinf = extinf.replace('#EXTINF:-1', f'#EXTINF:-1{logo_str}')
            else:
                extinf = f'#EXTINF:-1 tvg-id="{c["mapped_epg_id"]}"{logo_str} tvg-chno="{c["GuideNumber"]}", {c["GuideName"]}'
        else:
            if not extinf:
                extinf = f'#EXTINF:-1 tvg-chno="{c["GuideNumber"]}", {c["GuideName"]}'
                
        lines.append(extinf)
        lines.append(f"http://{request.host}{c['URL']}")
        
    return Response("\n".join(lines), mimetype='audio/x-mpegurl')

@app.route('/auto/v<channel_id>')
def stream(channel_id):
    channel = next((c for c in channels_db if c["GuideNumber"] == channel_id), None)
    if not channel:
        return "Channel not found", 404
        
    for ace_id in channel["ace_ids"]:
        stream_url = f"http://acexy:8080/ace/getstream?id={ace_id}"
        logging.info(f"Attempting to proxy channel {channel_id} from {stream_url}")
        try:
            req = requests.get(stream_url, stream=True, timeout=15)
            if req.status_code == 200:
                return Response(stream_with_context(req.iter_content(chunk_size=1024 * 1024)), content_type=req.headers.get('Content-Type', 'video/MP2T'))
            else:
                logging.warning(f"Fallback: {stream_url} returned {req.status_code}")
        except Exception as e:
            logging.error(f"Fallback: failed to connect to {stream_url}: {e}")
            continue
            
    logging.error(f"All sources for channel {channel_id} failed!")
    return "All stream sources failed", 502

if __name__ == '__main__':
    init_scheduler()
    app.run(host='0.0.0.0', port=5004)

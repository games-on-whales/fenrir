import time
import sys
import requests_unixsocket
import logging
import os
import urllib.parse

class WolfAPI:
    def __init__(self, socket_path: str):
        self.session = requests_unixsocket.Session()
        self.session.mount("http+unix://", requests_unixsocket.UnixAdapter())  # <== Fix here
        self.socket_path = "http+unix://" + urllib.parse.quote(socket_path, safe="")
        logging.debug(f"Using socket path: {self.socket_path}")

        self.config = {
            "video_width": int(os.environ.get("VIDEO_WIDTH", 1920)),
            "video_height": int(os.environ.get("VIDEO_HEIGHT", 1080)),
            "video_refresh_rate": int(os.environ.get("VIDEO_REFRESH_RATE", 60)),
            "audio_channel_count": int(os.environ.get("AUDIO_CHANNEL_COUNT", 2)),
            "client_ip": os.environ.get("CLIENT_IP", "10.128.1.0"),
            "client_id": os.environ.get("CLIENT_ID", "4193251087262667199"),
            "aes_key_path": os.environ.get("AES_KEY_PATH", ""),
            "aes_iv_path": os.environ.get("AES_IV_PATH", ""),
            "video_port": int(os.environ.get("VIDEO_PORT", 0)),
            "audio_port": int(os.environ.get("AUDIO_PORT", 0)),
            "wait_for_ping": int(os.environ.get("WAIT_FOR_PING", 0)) == 1,
        }

    def create_session(self):
        aes_key = ""
        aes_iv = ""

        if self.config["aes_key_path"]:
            with open(self.config["aes_key_path"], "r") as f:
                aes_key = f.read()
        if self.config["aes_iv_path"]:
            with open(self.config["aes_iv_path"], "r") as f:
                aes_iv = f.read()

        session_req = {
            "app_id": "1",
            "client_id": self.config["client_id"],
            "client_ip": self.config["client_ip"],
            "video_width": self.config["video_width"],
            "video_height": self.config["video_height"],
            "video_refresh_rate": self.config["video_refresh_rate"],
            "audio_channel_count": self.config["audio_channel_count"],
            "aes_key": aes_key,
            "aes_iv": aes_iv,
            "client_settings": {
                "run_uid": 1000,
                "run_gid": 1000,
                "controllers_override": [],
                "mouse_acceleration": 1.0,
                "v_scroll_acceleration": 1.0,
                "h_scroll_acceleration": 1.0,
            }
        }
        resp = self.session.post(self.socket_path + "/api/v1/sessions/add", json=session_req)
        resp.raise_for_status()  # Raise an error if response is not 2xx

        logging.debug(resp.json())
        return resp.json()["session_id"]

    def start_session(self, session_id: str):
        aes_key = ""
        aes_iv = ""

        if self.config["aes_key_path"]:
            with open(self.config["aes_key_path"], "r") as f:
                aes_key = f.read()
        if self.config["aes_iv_path"]:
            with open(self.config["aes_iv_path"], "r") as f:
                aes_iv = f.read()

        session_req = {
            "session_id": session_id,
            "video_session": {
                "display_mode": {
                    "width": self.config["video_width"],
                    "height": self.config["video_height"],
                    "refreshRate": self.config["video_refresh_rate"],
                },
                "gst_pipeline": "",
                "session_id": -1,  # Will be replaced by the server
                "port": self.config["video_port"],
                "wait_for_ping": self.config["wait_for_ping"],
                "timeout_ms": 999999999,
                "packet_size": -1,
                "frames_with_invalid_ref_threshold": -1,
                "fec_percentage": -1,
                "min_required_fec_packets": -1,
                "bitrate_kbps": -1,
                "slices_per_frame": -1,
                "color_range": "JPEG",
                "color_space": "BT601",
                "client_ip": self.config["client_ip"],
            },
            "audio_session": {
                "gst_pipeline": "",
                "session_id": -1,  # Will be replaced by the server
                "encrypt_audio": True,
                "aes_key": aes_key,
                "aes_iv": aes_iv,
                "wait_for_ping": self.config["wait_for_ping"],
                "port": self.config["audio_port"],
                "client_ip": self.config["client_ip"],
                "packet_duration": -1,
                "audio_mode": {
                    "channels": self.config["audio_channel_count"],
                    "streams": 1,
                    "coupled_streams": 1,
                    "speakers": ["FRONT_LEFT", "FRONT_RIGHT"],
                    "bitrate": 96000,
                    "sample_rate": 48000,
                }
            }
        }
        resp = self.session.post(self.socket_path + "/sessions/start", json=session_req)
        logging.debug(resp.json())
        resp.raise_for_status()
        return resp.json()



def main(argv):
    if len(argv) < 2:
        print("Usage: python wolf-agent.py /path/to/socket", file=sys.stderr)
        sys.exit(1)

    wolf_api_path = argv[1]
    logging.basicConfig(level=logging.DEBUG)

    wolf_api = WolfAPI(wolf_api_path)
    session_id = wolf_api.create_session()
    logging.debug(f"Session ID: {session_id}")

    if os.environ.get("START_SESSION", "0") == "1":
        logging.debug("Starting session")
        wolf_api.start_session(session_id)
        logging.debug("Session started")


if __name__ == "__main__":
    main(sys.argv)
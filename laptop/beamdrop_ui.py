#!/usr/bin/env python3
"""beamdrop's desktop window.

The protocol, pairing and transfers all live in the Go binary; this process
only draws and clicks, talking to it over the loopback JSON API. Splitting
it that way is what lets the window be written in something with real
stylesheets — the Go GUI toolkits available here could be made tidy but not
made to look like this.

The Go side is started as a child process if it is not already running, so
launching the app is one action rather than "first start the server".
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path

from PySide6.QtCore import Qt, QThread, QSize, Signal, QTimer
from PySide6.QtGui import QColor, QFont, QFontMetrics, QIcon, QKeySequence, QPixmap
from PySide6.QtWidgets import (
    QApplication, QFileDialog, QFrame, QHBoxLayout, QLabel, QLineEdit,
    QListWidget, QListWidgetItem, QMenu, QMessageBox, QPushButton, QScrollArea,
    QSizePolicy, QSplitter, QVBoxLayout, QWidget,
)

CONFIG_DIR = Path(os.environ.get("XDG_CONFIG_HOME", Path.home() / ".config")) / "beamdrop"
TOKEN_FILE = CONFIG_DIR / "upload.token"
DEFAULT_PORT = 4747

# --------------------------------------------------------------------------
# talking to the Go side
# --------------------------------------------------------------------------


class Api:
    def __init__(self, port: int = DEFAULT_PORT):
        self.base = f"http://127.0.0.1:{port}"
        self.token = ""

    def load_token(self) -> bool:
        try:
            self.token = TOKEN_FILE.read_text().strip()
        except OSError:
            return False
        return bool(self.token)

    def _call(self, path: str, payload=None, timeout=10):
        url = f"{self.base}{path}?token={self.token}"
        data = json.dumps(payload).encode() if payload is not None else None
        req = urllib.request.Request(url, data=data, method="POST" if data else "GET")
        with urllib.request.urlopen(req, timeout=timeout) as r:
            body = r.read()
        return json.loads(body) if body else None

    def state(self):
        return self._call("/api/state", timeout=5)

    def send_text(self, to: str, body: str):
        return self._call("/api/text", {"to": to, "body": body}, timeout=30)

    def send_file(self, to: str, path: str):
        # No timeout worth setting: a large file over a slow link legitimately
        # takes minutes, and the request does not return until the peer has
        # confirmed the hash.
        return self._call("/api/file", {"to": to, "path": path}, timeout=3600)

    def answer_pairing(self, req_id: str, accept: bool):
        return self._call("/api/pairing", {"id": req_id, "accept": accept})


def ensure_backend(api: Api) -> subprocess.Popen | None:
    """Start `beamdrop portal` unless something is already serving.

    An app that told you to go and start a server first would not have
    solved anything.
    """
    for _ in range(2):
        if api.load_token():
            try:
                api.state()
                return None  # already up, possibly started by hand
            except (urllib.error.URLError, OSError, ValueError):
                pass
        break

    # Keep the engine's output. Sending it to /dev/null meant a backend
    # that refused to start left nothing at all to look at.
    log_dir = Path(os.environ.get("XDG_CACHE_HOME", Path.home() / ".cache")) / "beamdrop"
    log_dir.mkdir(parents=True, exist_ok=True)
    log = open(log_dir / "portal.log", "ab", buffering=0)

    exe = shutil.which("beamdrop") or "beamdrop"
    cmd = [exe, "portal"]
    # A relay address in the config dir makes the laptop dial the relay and
    # stay connected, so messages sent here reach the phone through it. The
    # file is the whole config: write the relay's tailnet IP:port into it.
    relay = CONFIG_DIR / "relay.addr"
    if relay.is_file():
        addr = relay.read_text().strip()
        if addr:
            cmd += ["--connect-to", addr]
    try:
        proc = subprocess.Popen(
            cmd,
            stdout=log, stderr=log, stdin=subprocess.DEVNULL,
            start_new_session=True,
        )
    except OSError as e:
        log.write(f"could not run {exe}: {e}\n".encode())
        return None
    # The token is written on first start, so it may not exist until now.
    for _ in range(60):
        time.sleep(0.25)
        if api.load_token():
            try:
                api.state()
                return proc
            except (urllib.error.URLError, OSError, ValueError):
                continue
    return proc


class Poller(QThread):
    """Polls state off the GUI thread.

    Every network call has to be off this thread. The Go app froze for
    exactly this class of reason, and a request that blocks the event loop
    is indistinguishable to a user from a crash.
    """

    got = Signal(dict)
    failed = Signal(str)

    def __init__(self, api: Api):
        super().__init__()
        self.api = api
        self._running = True

    def run(self):
        while self._running:
            try:
                self.got.emit(self.api.state())
            except Exception as e:  # noqa: BLE001 - any failure is "backend down"
                self.failed.emit(str(e))
            self.msleep(1000)

    def stop(self):
        self._running = False
        self.wait(2000)


class Sender(QThread):
    """One send, off the GUI thread. See Poller."""

    done = Signal(bool, str)

    def __init__(self, api: Api, kind: str, to: str, payload: str):
        super().__init__()
        self.api, self.kind, self.to, self.payload = api, kind, to, payload

    def run(self):
        try:
            if self.kind == "text":
                self.api.send_text(self.to, self.payload)
            else:
                self.api.send_file(self.to, self.payload)
            self.done.emit(True, "")
        except urllib.error.HTTPError as e:
            self.done.emit(False, e.read().decode(errors="replace").strip() or str(e))
        except Exception as e:  # noqa: BLE001
            self.done.emit(False, str(e))


# --------------------------------------------------------------------------
# look
# --------------------------------------------------------------------------

QSS = """
* { font-family: "Inter", "Ubuntu", "Cantarell", sans-serif; }
QWidget#root { background: #ffffff; }

QWidget#sidebar { background: #f7f8fa; border-right: 1px solid #e6e8ec; }
QLabel#brand { font-size: 15px; font-weight: 700; color: #16181d; padding: 14px 16px 10px; }

QListWidget#devices { background: transparent; border: 0; outline: 0; padding: 4px 8px; }
QListWidget#devices::item { border-radius: 8px; padding: 0px; margin: 2px 0; }
QListWidget#devices::item:selected { background: #e8f3ec; }
QListWidget#devices::item:hover { background: #eef0f3; }

QWidget#header { background: #ffffff; border-bottom: 1px solid #e6e8ec; }
QLabel#headName { font-size: 15px; font-weight: 600; color: #16181d; }
QLabel#headMeta { font-size: 12px; color: #79808c; }

QScrollArea#feed { background: #fbfcfd; border: 0; }
QWidget#feedInner { background: #fbfcfd; }

QFrame#card { background: #ffffff; border: 1px solid #e6e8ec; border-radius: 14px; }
QFrame#cardMine { background: #e9f6ee; border: 1px solid #cfe9da; border-radius: 14px; }
QLabel#body { font-size: 14px; color: #16181d; }
QLabel#meta { font-size: 11px; color: #8a919c; }

QWidget#composer { background: #ffffff; border-top: 1px solid #e6e8ec; }
QLineEdit#entry {
    background: #f4f5f7; border: 1px solid #e2e5ea; border-radius: 18px;
    padding: 9px 14px; font-size: 14px; color: #16181d;
}
QLineEdit#entry:focus { border-color: #2ea062; background: #ffffff; }

QPushButton#round {
    background: #f0f1f4; border: 0; border-radius: 18px; font-size: 17px; color: #4a515c;
    min-width: 36px; max-width: 36px; min-height: 36px; max-height: 36px;
}
QPushButton#round:hover { background: #e4e6ea; }
QPushButton#send {
    background: #2ea062; border: 0; border-radius: 18px; color: #ffffff; font-size: 16px;
    min-width: 36px; max-width: 36px; min-height: 36px; max-height: 36px;
}
QPushButton#send:hover { background: #268653; }

QLabel#status { color: #8a919c; font-size: 11px; padding: 4px 14px 8px; }
QLabel#empty { color: #9aa1ac; font-size: 13px; }
QLabel#emptyTitle { color: #16181d; font-size: 16px; font-weight: 600; }
QLabel#qr { background: #ffffff; border: 1px solid #e6e8ec; border-radius: 10px; padding: 10px; }
QLabel#addr { color: #16181d; font-size: 13px; font-family: monospace; }
QPushButton#smallBtn {
    background: #eef0f3; color: #3a4049; border: 0; border-radius: 7px;
    padding: 5px 11px; font-size: 12px;
}
QPushButton#smallBtn:hover { background: #e2e5ea; }
QLabel#dropHint {
    background: rgba(46,160,98,0.10); color: #1e7a48; font-size: 17px; font-weight: 600;
    border: 2px dashed #2ea062; border-radius: 12px;
}

QFrame#pairBar { background: #fff8e6; border: 1px solid #f0e0b0; border-radius: 10px; }
QLabel#pairText { color: #6b5a20; font-size: 13px; }
QPushButton#pairYes {
    background: #2ea062; color: #fff; border: 0; border-radius: 8px; padding: 6px 14px; font-size: 13px;
}
QPushButton#pairNo {
    background: #ecedf0; color: #3a4049; border: 0; border-radius: 8px; padding: 6px 14px; font-size: 13px;
}

QSplitter::handle { background: #e6e8ec; width: 1px; }
QScrollBar:vertical { background: transparent; width: 8px; margin: 0; }
QScrollBar::handle:vertical { background: #d3d7de; border-radius: 4px; min-height: 30px; }
QScrollBar::add-line, QScrollBar::sub-line { height: 0; }
"""


def dot(colour: str, size: int = 9) -> QPixmap:
    """A coloured circle. Qt has no primitive for one, and an emoji renders
    differently on every machine."""
    pm = QPixmap(size * 2, size * 2)
    pm.fill(Qt.transparent)
    from PySide6.QtGui import QPainter, QBrush
    p = QPainter(pm)
    p.setRenderHint(QPainter.Antialiasing)
    p.setBrush(QBrush(QColor(colour)))
    p.setPen(Qt.NoPen)
    p.drawEllipse(0, 0, size * 2, size * 2)
    p.end()
    return pm.scaled(size, size, Qt.KeepAspectRatio, Qt.SmoothTransformation)


def qr_pixmap(data: str, px: int = 150) -> QPixmap | None:
    """A QR of the phone URL.

    Typing 100.64.0.1:4747 into a phone keyboard is the single most
    annoying step in setting this up, and it is the step you repeat every
    time the tab gets closed. Painted from the matrix rather than rendered
    to an image file, so there is no Pillow dependency.
    """
    try:
        import qrcode
    except ImportError:
        return None
    q = qrcode.QRCode(box_size=1, border=2)
    q.add_data(data)
    q.make(fit=True)
    m = q.get_matrix()
    n = len(m)
    scale = max(1, px // n)
    size = n * scale
    pm = QPixmap(size, size)
    pm.fill(QColor("#ffffff"))
    from PySide6.QtGui import QPainter
    p = QPainter(pm)
    p.setPen(Qt.NoPen)
    p.setBrush(QColor("#16181d"))
    for y, row in enumerate(m):
        for x, on in enumerate(row):
            if on:
                p.drawRect(x * scale, y * scale, scale, scale)
    p.end()
    return pm


def human(n: int) -> str:
    for limit, suffix, div in ((1 << 30, "GB", 1 << 30), (1 << 20, "MB", 1 << 20), (1 << 10, "KB", 1 << 10)):
        if n >= limit:
            return f"{n / div:.1f} {suffix}" if div > (1 << 10) else f"{round(n / div)} {suffix}"
    return f"{n} B"


def ago(unix: int) -> str:
    d = max(0, int(time.time()) - unix)
    if d < 60:
        return "just now"
    if d < 3600:
        return f"{d // 60}m ago"
    if d < 86400:
        return f"{d // 3600}h ago"
    return f"{d // 86400}d ago"


# --------------------------------------------------------------------------
# widgets
# --------------------------------------------------------------------------


class DeviceRow(QWidget):
    def __init__(self, name: str, sub: str, online: bool, glyph: str):
        super().__init__()
        lay = QHBoxLayout(self)
        lay.setContentsMargins(10, 8, 10, 8)
        lay.setSpacing(10)

        icon = QLabel(glyph)
        icon.setFixedWidth(18)
        icon.setStyleSheet("font-size:15px;")
        lay.addWidget(icon)

        text = QVBoxLayout()
        text.setSpacing(0)
        n = QLabel(name)
        n.setStyleSheet("font-size:13px;font-weight:600;color:#16181d;")
        s = QLabel(sub)
        s.setStyleSheet("font-size:11px;color:#8a919c;")
        text.addWidget(n)
        text.addWidget(s)
        lay.addLayout(text, 1)

        d = QLabel()
        d.setPixmap(dot("#2ea062" if online else "#c3c8d0"))
        lay.addWidget(d)


class Bubble(QWidget):
    """One entry in the conversation: a message, or a file with its preview."""

    def __init__(self, msg: dict, on_open):
        super().__init__()
        outer = QHBoxLayout(self)
        outer.setContentsMargins(14, 3, 14, 3)

        card = QFrame()
        card.setObjectName("cardMine" if msg.get("outbound") else "card")
        card.setMaximumWidth(520)
        inner = QHBoxLayout(card)
        inner.setContentsMargins(12, 10, 14, 10)
        inner.setSpacing(11)

        if msg.get("kind") == "file" and msg.get("is_image") and msg.get("path"):
            thumb = QLabel()
            pm = QPixmap(msg["path"])
            if not pm.isNull():
                # Scaled once, here. Qt keeps the scaled pixmap; nothing
                # re-reads the file on redraw.
                thumb.setPixmap(pm.scaled(56, 56, Qt.KeepAspectRatioByExpanding,
                                          Qt.SmoothTransformation))
                thumb.setFixedSize(56, 56)
                thumb.setStyleSheet("border-radius:8px;")
                inner.addWidget(thumb)

        text = QVBoxLayout()
        text.setSpacing(2)
        if msg.get("kind") == "file":
            body = QLabel(msg.get("file_name", ""))
            meta = QLabel(f"{human(msg.get('size', 0))} · {direction(msg)} · {ago(msg.get('at', 0))}")
        else:
            text_body = msg.get("text", "")
            body = QLabel(text_body)
            body.setWordWrap(True)
            # A word-wrapped QLabel reports a tiny minimum width, so the card
            # collapsed and short messages wrapped onto two lines for no
            # reason. Ask the font how wide the text actually is and let the
            # card be that wide, up to a comfortable reading measure.
            # Measured with an explicit font: the stylesheet's 14px is not
            # applied until the widget is polished, so asking the label at
            # construction time returns the default font and a width that is
            # too small.
            probe = QFont(body.font())
            probe.setPixelSize(14)
            natural = QFontMetrics(probe).horizontalAdvance(text_body)
            body.setMinimumWidth(min(natural + 10, 420))
            meta = QLabel(f"{direction(msg)} · {ago(msg.get('at', 0))}")
        body.setObjectName("body")
        meta.setObjectName("meta")
        text.addWidget(body)
        text.addWidget(meta)
        inner.addLayout(text, 1)

        if msg.get("outbound"):
            outer.addStretch(1)
            outer.addWidget(card)
        else:
            outer.addWidget(card)
            outer.addStretch(1)

        if msg.get("kind") == "file" and msg.get("path"):
            card.setCursor(Qt.PointingHandCursor)
            card.setToolTip(msg["path"])
            card.mouseReleaseEvent = lambda _e, p=msg["path"]: on_open(p)
            # Right-click for the things you actually want to do with a file
            # you were just sent, rather than only "open it in something".
            card.setContextMenuPolicy(Qt.CustomContextMenu)
            card.customContextMenuRequested.connect(
                lambda pos, c=card, m=msg: file_menu(c, pos, m))
        elif msg.get("kind") == "text":
            # Selectable, so a link or a code someone sent can be copied
            # instead of retyped.
            body.setTextInteractionFlags(Qt.TextSelectableByMouse)
            card.setContextMenuPolicy(Qt.CustomContextMenu)
            card.customContextMenuRequested.connect(
                lambda pos, c=card, m=msg: text_menu(c, pos, m))


def file_menu(card: QWidget, pos, msg: dict):
    path = Path(msg["path"])
    menu = QMenu(card)
    menu.addAction("Open", lambda: _open(path))
    menu.addAction("Show in folder", lambda: _reveal(path))
    menu.addAction("Copy file", lambda: _copy_file(path))
    menu.addAction("Copy path", lambda: QApplication.clipboard().setText(str(path)))
    menu.addSeparator()
    menu.addAction("Save a copy…", lambda: _save_as(card, path))
    menu.exec(card.mapToGlobal(pos))


def text_menu(card: QWidget, pos, msg: dict):
    menu = QMenu(card)
    menu.addAction("Copy text", lambda: QApplication.clipboard().setText(msg.get("text", "")))
    menu.exec(card.mapToGlobal(pos))


def _open(path: Path):
    subprocess.Popen(["xdg-open", str(path)], stdout=subprocess.DEVNULL,
                     stderr=subprocess.DEVNULL, start_new_session=True)


def _reveal(path: Path):
    # Most desktops understand this; falling back to opening the directory
    # is better than doing nothing on the ones that do not.
    try:
        subprocess.Popen(["dbus-send", "--session", "--dest=org.freedesktop.FileManager1",
                          "--type=method_call", "/org/freedesktop/FileManager1",
                          "org.freedesktop.FileManager1.ShowItems",
                          f"array:string:file://{path}", "string:"],
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    except OSError:
        _open(path.parent)


def _copy_file(path: Path):
    """Put the file on the clipboard so it can be pasted into a file manager
    or a chat, not just its name."""
    from PySide6.QtCore import QMimeData, QUrl
    md = QMimeData()
    md.setUrls([QUrl.fromLocalFile(str(path))])
    md.setText(str(path))
    QApplication.clipboard().setMimeData(md)


def _save_as(parent: QWidget, path: Path):
    dest, _ = QFileDialog.getSaveFileName(parent, "Save a copy", str(Path.home() / path.name))
    if dest:
        try:
            shutil.copy2(path, dest)
        except OSError as e:
            QMessageBox.warning(parent, "beamdrop", f"Could not save: {e}")


def direction(msg: dict) -> str:
    peer = msg.get("peer") or ""
    if msg.get("outbound"):
        return f"to {peer}" if peer else "sent"
    return f"from {peer}" if peer else "received"


# --------------------------------------------------------------------------
# the window
# --------------------------------------------------------------------------


class Window(QWidget):
    def __init__(self, api: Api):
        super().__init__()
        self.api = api
        self.selected = ""      # peer name; "" is everyone
        self.state: dict = {}
        self.senders: list[Sender] = []
        self._feed_sig = None   # so an unchanged feed is not rebuilt

        self.setObjectName("root")
        self.setWindowTitle("beamdrop")
        self.resize(940, 640)

        root = QHBoxLayout(self)
        root.setContentsMargins(0, 0, 0, 0)
        root.setSpacing(0)

        # --- sidebar
        side = QWidget()
        side.setObjectName("sidebar")
        side.setFixedWidth(230)
        sl = QVBoxLayout(side)
        sl.setContentsMargins(0, 0, 0, 0)
        sl.setSpacing(0)
        brand = QLabel("beamdrop")
        brand.setObjectName("brand")
        sl.addWidget(brand)
        self.devices = QListWidget()
        self.devices.setObjectName("devices")
        self.devices.currentRowChanged.connect(self.on_device)
        sl.addWidget(self.devices, 1)
        root.addWidget(side)

        # --- main column
        main = QVBoxLayout()
        main.setContentsMargins(0, 0, 0, 0)
        main.setSpacing(0)

        head = QWidget()
        head.setObjectName("header")
        hl = QHBoxLayout(head)
        hl.setContentsMargins(16, 12, 16, 12)
        hl.setSpacing(10)
        self.head_dot = QLabel()
        self.head_dot.setPixmap(dot("#c3c8d0"))
        hl.addWidget(self.head_dot)
        ht = QVBoxLayout()
        ht.setSpacing(1)
        self.head_name = QLabel("beamdrop")
        self.head_name.setObjectName("headName")
        self.head_meta = QLabel("starting…")
        self.head_meta.setObjectName("headMeta")
        ht.addWidget(self.head_name)
        ht.addWidget(self.head_meta)
        hl.addLayout(ht, 1)
        main.addWidget(head)

        # --- pairing prompt, hidden until one arrives
        self.pair_bar = QFrame()
        self.pair_bar.setObjectName("pairBar")
        pb = QHBoxLayout(self.pair_bar)
        pb.setContentsMargins(14, 10, 14, 10)
        pb.setSpacing(10)
        self.pair_text = QLabel()
        self.pair_text.setObjectName("pairText")
        pb.addWidget(self.pair_text, 1)
        yes = QPushButton("Connect")
        yes.setObjectName("pairYes")
        yes.clicked.connect(lambda: self.answer_pair(True))
        no = QPushButton("Refuse")
        no.setObjectName("pairNo")
        no.clicked.connect(lambda: self.answer_pair(False))
        pb.addWidget(yes)
        pb.addWidget(no)
        self.pair_bar.hide()
        wrap = QWidget()
        wl = QVBoxLayout(wrap)
        wl.setContentsMargins(14, 10, 14, 0)
        wl.addWidget(self.pair_bar)
        main.addWidget(wrap)
        self.pair_wrap = wrap
        wrap.hide()

        # --- feed
        self.scroll = QScrollArea()
        self.scroll.setObjectName("feed")
        self.scroll.setWidgetResizable(True)
        self.scroll.setHorizontalScrollBarPolicy(Qt.ScrollBarAlwaysOff)
        self.feed_inner = QWidget()
        self.feed_inner.setObjectName("feedInner")
        self.feed_lay = QVBoxLayout(self.feed_inner)
        self.feed_lay.setContentsMargins(0, 10, 0, 10)
        self.feed_lay.setSpacing(0)
        self.feed_lay.addStretch(1)
        self.empty = self.build_empty()
        self.feed_lay.addWidget(self.empty)
        self.feed_lay.addStretch(1)
        self.scroll.setWidget(self.feed_inner)
        main.addWidget(self.scroll, 1)

        # --- composer
        comp = QWidget()
        comp.setObjectName("composer")
        cl = QHBoxLayout(comp)
        cl.setContentsMargins(14, 10, 14, 6)
        cl.setSpacing(9)
        attach = QPushButton("+")
        attach.setObjectName("round")
        attach.setCursor(Qt.PointingHandCursor)
        attach.setToolTip("Send a file")
        attach.clicked.connect(self.pick_file)
        cl.addWidget(attach)

        inbox_btn = QPushButton("🗀")
        inbox_btn.setObjectName("round")
        inbox_btn.setCursor(Qt.PointingHandCursor)
        inbox_btn.setToolTip("Open the folder received files go to")
        inbox_btn.clicked.connect(self.open_inbox)
        cl.addWidget(inbox_btn)
        self.entry = QLineEdit()
        self.entry.setObjectName("entry")
        self.entry.setPlaceholderText("Type a message, press enter to send")
        self.entry.returnPressed.connect(self.send_text)
        cl.addWidget(self.entry, 1)
        send = QPushButton("↑")
        send.setObjectName("send")
        send.setCursor(Qt.PointingHandCursor)
        send.clicked.connect(self.send_text)
        cl.addWidget(send)
        main.addWidget(comp)

        self.status = QLabel("")
        self.status.setObjectName("status")
        main.addWidget(self.status)

        root.addLayout(main, 1)
        self.setAcceptDrops(True)

        self.drop_hint = QLabel("Release to send", self)
        self.drop_hint.setObjectName("dropHint")
        self.drop_hint.setAlignment(Qt.AlignCenter)
        self.drop_hint.hide()

    def resizeEvent(self, e):
        super().resizeEvent(e)
        # The overlay is a child of the window rather than a layout item, so
        # it covers the whole thing without disturbing anything.
        self.drop_hint.setGeometry(0, 0, self.width(), self.height())

    def build_empty(self) -> QWidget:
        """What fills the window before anything has happened.

        This is the screen a new user spends the most time on, so it is the
        setup instructions rather than the word "empty": the QR to scan, the
        address to type if scanning is awkward, and a button to copy it.
        """
        w = QWidget()
        v = QVBoxLayout(w)
        v.setAlignment(Qt.AlignCenter)
        v.setSpacing(10)

        title = QLabel("Connect your phone")
        title.setObjectName("emptyTitle")
        title.setAlignment(Qt.AlignCenter)
        v.addWidget(title)

        self.qr = QLabel()
        self.qr.setAlignment(Qt.AlignCenter)
        self.qr.setObjectName("qr")
        v.addWidget(self.qr, 0, Qt.AlignCenter)

        self.qr_hint = QLabel("Scan this, or open the address below in Safari")
        self.qr_hint.setObjectName("empty")
        self.qr_hint.setAlignment(Qt.AlignCenter)
        v.addWidget(self.qr_hint)

        row = QHBoxLayout()
        row.setAlignment(Qt.AlignCenter)
        row.setSpacing(8)
        self.addr = QLabel("")
        self.addr.setObjectName("addr")
        self.addr.setTextInteractionFlags(Qt.TextSelectableByMouse)
        row.addWidget(self.addr)
        copy = QPushButton("Copy")
        copy.setObjectName("smallBtn")
        copy.setCursor(Qt.PointingHandCursor)
        copy.clicked.connect(self.copy_address)
        row.addWidget(copy)
        v.addLayout(row)
        return w

    def copy_address(self):
        QApplication.clipboard().setText(self.addr.text())
        self.status.setText("address copied")

    # --- data ---------------------------------------------------------

    def on_state(self, st: dict):
        self.state = st
        peers = st.get("peers") or []

        # Devices. Rebuilt only when the set changes, or selection would be
        # reset out from under a click every second.
        want = ["All devices"] + [p["name"] for p in peers]
        have = [self.devices.item(i).data(Qt.UserRole) for i in range(self.devices.count())]
        if want != have:
            keep = self.selected
            self.devices.blockSignals(True)
            self.devices.clear()
            for i, label in enumerate(want):
                item = QListWidgetItem()
                item.setData(Qt.UserRole, label)
                if i == 0:
                    sub = f"{len(peers)} connected"
                    row = DeviceRow("All devices", sub, bool(peers), "🖥")
                else:
                    row = DeviceRow(label, "connected", True, "📱")
                item.setSizeHint(QSize(0, row.sizeHint().height()))
                self.devices.addItem(item)
                self.devices.setItemWidget(item, row)
            idx = want.index(keep) if keep in want else 0
            self.devices.setCurrentRow(idx)
            self.devices.blockSignals(False)

        # Header
        if not peers:
            self.head_dot.setPixmap(dot("#d8a657"))
            self.head_name.setText("Waiting for a device")
            urls = st.get("urls") or []
            addr = trim_url(urls[1]) if len(urls) > 1 else ""
            self.head_meta.setText(f"open {addr} on your phone" if addr else "no network address")
            self.set_address(addr)
        else:
            self.head_dot.setPixmap(dot("#2ea062"))
            self.head_name.setText(self.selected or join_names([p["name"] for p in peers]))
            self.head_meta.setText(f"inbox {st.get('inbox_dir', '')}")
            urls = st.get("urls") or []
            self.set_address(trim_url(urls[1]) if len(urls) > 1 else "")

        # Pairing
        pending = st.get("pairing") or []
        if pending:
            p = pending[0]
            self.pending_pair = p
            self.pair_text.setText(
                f"<b>{p['peer']}</b> wants to connect — check the code matches: "
                f"<b>{p['code']}</b>")
            self.pair_wrap.show()
            self.pair_bar.show()
        else:
            self.pending_pair = None
            self.pair_wrap.hide()

        self.rebuild_feed(st.get("feed") or [])

        acts = st.get("activity") or []
        if acts:
            self.status.setText(acts[0])

    def set_address(self, addr: str):
        if addr == getattr(self, "_addr", None):
            return  # repainting a QR every second is pointless work
        self._addr = addr
        self.addr.setText(addr)
        pm = qr_pixmap(addr) if addr else None
        if pm:
            self.qr.setPixmap(pm)
            self.qr.show()
            self.qr_hint.show()
        else:
            self.qr.hide()
            self.qr_hint.setText("Open this address on your phone")

    def rebuild_feed(self, feed: list[dict]):
        if self.selected:
            # Received files carry no peer name — they are read back off
            # disk — so they show under every device rather than vanishing.
            feed = [m for m in feed if m.get("peer") in (self.selected, "", None)]
        sig = [(m.get("at"), m.get("kind"), m.get("text"), m.get("file_name")) for m in feed]
        if sig == self._feed_sig:
            return
        self._feed_sig = sig

        while self.feed_lay.count():
            item = self.feed_lay.takeAt(0)
            w = item.widget()
            if w and w is not self.empty:
                w.deleteLater()

        if not feed:
            self.feed_lay.addStretch(1)
            self.feed_lay.addWidget(self.empty)
            self.empty.show()
            self.feed_lay.addStretch(1)
            return

        self.empty.hide()
        # Stretch first, so a short conversation sits against the composer
        # the way every chat does, instead of stranded at the top with a
        # field of empty below it. With enough messages it collapses to
        # nothing and the scroll takes over.
        self.feed_lay.addStretch(1)
        for m in feed:
            self.feed_lay.addWidget(Bubble(m, self.open_path))
        self._scroll_to_bottom()

    def _scroll_to_bottom(self):
        # The scrollbar's maximum is only recomputed once the layout has
        # been told to resize; scrolling to the old maximum leaves the
        # newest message below the fold. Force the recompute, then jump.
        # Deferred so the layout pass that follows this rebuild has run.
        QTimer.singleShot(0, self._do_scroll)

    def _do_scroll(self):
        self.feed_inner.adjustSize()
        bar = self.scroll.verticalScrollBar()
        bar.setValue(bar.maximum())

    def showEvent(self, e):
        super().showEvent(e)
        # The first state poll can land before the window is shown, when the
        # scroll area has zero size and scrolling is a no-op. Once it is on
        # screen, make sure the newest message is in view.
        self._scroll_to_bottom()

    def on_failed(self, msg: str):
        self.head_dot.setPixmap(dot("#c3c8d0"))
        self.head_name.setText("beamdrop")
        self.head_meta.setText("backend not reachable")

    # --- actions ------------------------------------------------------

    def on_device(self, row: int):
        if row <= 0:
            self.selected = ""
        else:
            item = self.devices.item(row)
            self.selected = item.data(Qt.UserRole) if item else ""
        self._feed_sig = None  # force a redraw for the new filter
        self.on_state(self.state or {})

    def answer_pair(self, accept: bool):
        p = getattr(self, "pending_pair", None)
        if not p:
            return
        self.pair_wrap.hide()
        try:
            self.api.answer_pairing(p["id"], accept)
        except Exception as e:  # noqa: BLE001
            self.status.setText(f"pairing: {e}")

    def send_text(self):
        body = self.entry.text().strip()
        if not body:
            return
        if not (self.state.get("peers") or []):
            self.status.setText("no device is connected yet")
            return
        self.entry.clear()
        self.run_send("text", body)

    def open_inbox(self):
        d = (self.state or {}).get("inbox_dir")
        if d:
            self.open_path(d)

    def pick_file(self):
        path, _ = QFileDialog.getOpenFileName(self, "Send a file", str(Path.home()))
        if path:
            self.run_send("file", path)

    def run_send(self, kind: str, payload: str):
        self.status.setText(f"sending {Path(payload).name if kind == 'file' else 'message'}…")
        s = Sender(self.api, kind, self.selected, payload)
        s.done.connect(self.on_sent)
        # Held on self, or Python garbage-collects the thread mid-send.
        self.senders.append(s)
        s.finished.connect(lambda: self.senders.remove(s) if s in self.senders else None)
        s.start()

    def on_sent(self, ok: bool, err: str):
        self.status.setText("sent" if ok else f"failed: {err}")
        self._feed_sig = None

    def open_path(self, path: str):
        self.status.setText(f"opening {Path(path).name}…")
        subprocess.Popen(["xdg-open", path], stdout=subprocess.DEVNULL,
                         stderr=subprocess.DEVNULL, start_new_session=True)

    # --- drag and drop ------------------------------------------------

    def dragEnterEvent(self, e):
        if e.mimeData().hasUrls():
            e.acceptProposedAction()
            # Say the drop will be accepted. Without this the window looks
            # inert while you hover a file over it, and people let go
            # somewhere else.
            self.drop_hint.setText("Release to send")
            self.drop_hint.show()

    def dragLeaveEvent(self, e):
        self.drop_hint.hide()

    def dropEvent(self, e):
        self.drop_hint.hide()
        for url in e.mimeData().urls():
            if url.isLocalFile():
                self.run_send("file", url.toLocalFile())

    def keyPressEvent(self, e):
        # Ctrl+V sends whatever is on the clipboard — a copied file, or the
        # text. Reaching for the file picker to send something you have
        # already copied is a step that did not need to exist.
        if e.matches(QKeySequence.Paste):
            md = QApplication.clipboard().mimeData()
            if md.hasUrls():
                for url in md.urls():
                    if url.isLocalFile():
                        self.run_send("file", url.toLocalFile())
                return
            if md.hasText() and not self.entry.hasFocus():
                self.entry.setText(md.text())
                self.entry.setFocus()
                return
        super().keyPressEvent(e)


def trim_url(s: str) -> str:
    s = s.strip()
    i = s.find("  ")
    return s[:i].strip() if i > 0 else s


def join_names(names: list[str]) -> str:
    if not names:
        return ""
    if len(names) == 1:
        return names[0]
    if len(names) == 2:
        return f"{names[0]} and {names[1]}"
    return f"{names[0]} and {len(names) - 1} others"


def main() -> int:
    app = QApplication(sys.argv)
    app.setApplicationName("beamdrop")
    app.setDesktopFileName("beamdrop")
    app.setStyleSheet(QSS)

    api = Api()
    child = ensure_backend(api)
    if not api.token:
        QMessageBox.critical(None, "beamdrop",
                             "Could not start the beamdrop backend.\n\n"
                             "Check that `beamdrop` is on your PATH.")
        return 1

    win = Window(api)
    poller = Poller(api)
    poller.got.connect(win.on_state)
    poller.failed.connect(win.on_failed)
    poller.start()
    win.show()

    rc = app.exec()
    poller.stop()
    # Only stop the backend if this process started it. A portal the user
    # already had running is not ours to kill.
    if child is not None:
        child.terminate()
    return rc


if __name__ == "__main__":
    sys.exit(main())

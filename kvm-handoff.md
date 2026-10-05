# 引き継ぎ書: 自作 IP-KVM の映像取得検証 (LicheeRV Nano)

> **方針変更 (2026-10-04):** 処理能力とシステムの費用から、Linux PC をホストにして、キャプチャボードと ESP32 を PC につなぐ構成を先に完成させる。LicheeRV Nano への組み込みは、PC 版を品質的に完成させてから取り組む。この書面は、そのときのための Nano の検証記録として残す。

## 目的

Linux 上のシステムから、Windows PC の画面を Web ブラウザで見ながらキーボードとマウスで操作できるようにする (PiKVM 相当を安価に作る)。全体設計は `kvm-design.md` を参照する。この書面は、そのうち LicheeRV Nano でのキャプチャ映像の取得が現在の担当範囲であることを前提に、現状と次の作業を書く。

構成の要点は次のとおり。

- 映像: Windows PC の HDMI 出力 → USB キャプチャボード → Nano の Type-C (Nano が USB ホスト)。
- 入力: ESP32-S3 が Windows PC に USB HID (キーボード、マウス) として見える。Nano と ESP32 は UART で繋ぐ。ESP32 側は未着手で、今回の検証では使っていない。

## 検証環境

- 基板は LicheeRV Nano の E 版 (Ethernet 版、Wi-Fi と Bluetooth のチップなし)。
- 電源は、安定化電源から 5V ピン (VSYS、VBUS) と GND に 5.0V を給電している。
- Type-C に USB キャプチャボード (MS2109 系、ID 534d:2109) を接続している。HDMI 入力は Windows PC の画面。
- ネットワークは LAN ケーブルで接続している。最後に確認した IP は 192.168.10.177 (DHCP のため変わる可能性がある)。
- SSH は root で入る。パスワードはユーザーが持っているので、ユーザーから受け取る。この書面や作成するファイルには書かない。
- Nano の USB ホストモード中は、Type-C 経由の SSH (usb0) は使えない。LAN で入る。

## 確認済みの事実

### OS

- アーキテクチャは riscv64。公式イメージ相当で、`/usr/bin/ffmpeg` (libavformat 58.76) が入っている。`v4l2-ctl` は入っていない。
- Linux が使えるメモリは合計 128MB。アイドル時の空きは約 65MB。
- カーネルには dwc2 (USB コントローラ)、uvcvideo、usb-storage、usbhid、cdc_acm、snd-usb-audio が入っている。

### USB の役割切替

- `/proc/cviusb/otg_role` に `host` か `device` を書き込んで切り替える。
- 起動時の `/etc/init.d/S08usbdev` が、`/boot/usb.host` があれば host、`/boot/usb.dev` があればガジェットを組み立てて device にする。
- 再起動なしで host にするには `/etc/init.d/S08usbdev stop` を実行する。
- 現在は恒久化していない。再起動すると device に戻る。

### キャプチャボード

- ホストモードで、High Speed の UVC として認識される。映像は `/dev/video0`。
- 対応形式は MJPEG と YUYV。解像度は 1920x1080、1600x1200、1360x768、1280x1024、1280x960、1280x720、1024x768、800x600、720x576、720x480、640x480。
- 取得結果は、次のコマンド (`-c copy`、再エンコードなし) による。
  - 640x480、30fps: 5 秒間安定した。約 11.7Mbps、1 フレームは約 47KB。
  - 1280x720、30fps: 取得できたが、約 1.1 秒の時点から 2.8 秒間フレームが止まった。空きメモリは 2MB まで下がった。
  - 1920x1080: ストリーム開始の瞬間に SSH が切れ、基板が再起動している (`otg_role` が device に戻っていた)。
- 取得中の消費電力は約 1.7W。電源は原因ではない。

### 原因の推測

メモリ不足と推測している。UVC ドライバが V4L2 のバッファを最大 32 個確保し、1 個が「幅 × 高さ × 2 バイト」相当だとすると、720p で約 56MB (実測の増加約 58MB と一致)、1080p で約 127MB になり、空きを超える。バッファのサイズと個数の前提は、まだ確認していない。

### 起動時のログで無視してよいもの

- `aicbsp: ... fail to set AIC_WIFI power state`: E 版には Wi-Fi チップがないため出る。
- `hid-generic ... device has no listeners`: キャプチャボードの HID インターフェースを受け取るプログラムがないだけ。

## 次の作業

1. SSH で接続し、`cat /proc/cviusb/otg_role` を確認する。device なら `/etc/init.d/S08usbdev stop` で host にする。
2. V4L2 のバッファ数を 4 個程度に絞った、MJPEG の取得プログラムを作る。ffmpeg の v4l2 入力でバッファ数を指定できるかは未確認。指定できなければ、自前の取得プログラムか ustreamer を使う。
3. 段階的に試す。640x480、1280x720、1920x1080 の順に進め、各段階でメモリの空きを見る。
4. 1080p30 で 30 秒以上取得し、フレームの欠けと空きメモリの推移を確認する。静止画面だけでなく、Windows で動画を再生するなど動きのある映像でも確認する。
5. 取得したフレームを、再エンコードせずに HTTP の `multipart/x-mixed-replace` で配信する。ブラウザでは `<img>` で表示できる。
6. 以降の入力側 (ESP32 の HID、Web UI からのキーボードとマウス操作) は `kvm-design.md` の実装順序に従う。

### 検証用コマンド

メモリの監視 (別の SSH で実行する):

```
while true; do free -m | sed -n 2p; sleep 0.5; done
```

ffmpeg での取得 (720p の例):

```
ffmpeg -hide_banner -f v4l2 -input_format mjpeg -video_size 1280x720 -framerate 30 -i /dev/video0 -c copy -t 5 <保存先>
```

終了時に `Some buffers are still owned` と `VIDIOC_QBUF` のエラーが出ても、1 枚で止めたときの終了処理なので無視してよい。

## 注意

- 高解像度の試験で基板が落ちる可能性がある。落ちたときは SSH が切れる。再接続を待ち、`otg_role` が device に戻っていれば host にし直す。電源の再投入が必要な場合は、ユーザーに依頼する。
- 保存先は `/tmp` 以外にする。`/tmp` はメモリ上のファイルシステムで、保存した分だけ空きメモリを消費する。SD カード上のディレクトリを使い、`df` で容量を確認する。
- バイナリは静的リンクで作る。Go なら `CGO_ENABLED=0 GOOS=linux GOARCH=riscv64`。C の場合、Nano の libc の種類 (musl か glibc か) は未確認。`ls /lib` などで確認する。
- 5V ピンに給電している間、Type-C に PC などの電源を挿さない (5V が並列になる)。
- 物理操作 (配線、電源、HDMI の接続) はユーザーが行う。

## 実行の方針

- 読み取り専用のコマンドと、`S08usbdev stop` (再起動で戻る) は、確認なしで実行してよい。
- `/boot` のフラグファイルの変更、`reboot`、SD カードへの書き込みは、ユーザーに確認してから実行する。`/boot/usb.host` を作る場合、LAN で SSH できることが前提になる。入れなくなったときは、SD カードを PC に挿して `/boot` のフラグファイルを直せば復旧できる。

## ユーザーに確認すること

- 実装言語 (Go か C)。手元の PC で riscv64 向けのクロスコンパイルができるか。
- ESP32-S3 (Freenove の 2 ポート版) の到着状況と、HID 側に着手する時期。

## 作業結果 (2026-10-04)

### 原因の確定

- メモリ: 以前の 1080p での落ちは、`uvc_request_buffers` 内で OOM killer が ffmpeg と sshd を kill したため (dmesg で確認)。V4L2 のバッファ 1 個は `sizeimage` = 幅 × 高さ × 2 (1080p で 4147200 バイト) で、推測どおりだった。バッファを 4 個にすると、1080p でもメモリの増加は約 16MB で済み、落ちなくなった。
- CPU: 標準の uvcvideo では、1080p30 にしても実際は 17〜20fps で、フレームの欠けも出る。CPU の消費は「約 18% の固定分 + 約 1.7% / Mbps」で、約 45Mbps で飽和する。カーネルは `CONFIG_DMA_NONCOHERENT` なしで `DMA_DIRECT_REMAP=y` のため、5.10 の uvcvideo は URB バッファを `usb_alloc_coherent` (非キャッシュ) に確保し、そこから memcpy する。そのため遅い。uvcvideo は組み込み (=y) で、差し替えられない。
- `/proc/stat` の CPU 使用率は、USB 割り込みとタイマ tick の位相の関係で 3% と 64% のように不安定になり、信用できない。空き CPU は、`kvmcap -spin` (nice 19 の空回りの回転数) で測る。アイドル時の基準値は約 16740/秒。

### 対策: usbfs による自前の UVC 取得

`kvmcap/` (Go、静的リンク、`CGO_ENABLED=0 GOOS=linux GOARCH=riscv64`)。

- `-backend usbfs` (既定): 起動時に uvcvideo を unbind し、`/dev/bus/usb` から UVC の probe/commit を行い、isochronous の URB (8 本 × 32 パケット) を自前で回して MJPEG のフレームを組み立てる。終了時に uvcvideo を bind し直す。音声と HID のインターフェースには触れない。
- `-backend v4l2`: `/dev/video0` から、バッファ数を指定して取得する (`-bufs`)。
- `-t 60s`: 試験モード (1 秒ごとの fps、Mbps、欠け、空きメモリを表示)。`-o` で MJPEG を保存する。
- `-t` なし: HTTP サーバー (`:8081`)。`/` に `<img>` のページ、`/stream` に multipart/x-mixed-replace、`/snapshot.jpg`、`/stats`。

| 条件 (YouTube の動画を再生中) | fps | 欠け | 空き CPU | メモリの増加 |
|---|---|---|---|---|
| v4l2 1080p30 (4 バッファ) | 17〜20 | あり | 約 5% | 約 16MB |
| usbfs 720p30 (alt 2) | 30.0 | 0 | 約 71% | 約 3MB |
| usbfs 1080p30 (alt 3)、60 秒 | 29.97 (1799 フレーム) | 0 | 約 66% | 約 3MB (空きは約 87MB で一定) |
| usbfs 1080p30 + HTTP 1 クライアント | 約 29.7 | 12 秒間で 1 | ほぼ 0 | — |
| usbfs 1080p30 + HTTP 2 クライアント | 各 19〜20 | 取得側で 14 | 0 | — |

- 取得したフレームは、ffprobe ですべて 1920x1080 の JPEG として復号できた。
- 1080p30 の MJPEG は約 70Mbps。eth0 は 100Mbps のリンクのため、1080p30 は 1 クライアントが上限。CPU にも余裕がない。

### 運用 (2026-10-04 時点)

> **2026-10-04 に Nano を素の状態に戻した。** kvmcap、S95kvmcap、`/boot/usb.host` を削除し、device モードに戻っている。以下は、再び載せ替えるときの手順として残す。ソースは `kvmcap/` にある。

- バイナリは `/root/kvmcap/kvmcap`。起動スクリプトは `/etc/init.d/S95kvmcap` (start/stop/restart)。起動時に、1080p30 で `:8081` の配信を始める。ログは syslog (`/var/log/messages`。`/var/log` は `/tmp` へのリンクで、メモリ上)。
- host モードは `/boot/usb.host` で恒久化した。`usb.dev` も残っているが、S08usbdev は `usb.host` を先に見るので host になる。device に戻すには `/boot/usb.host` を消す。
- Web UI (`http://<Nano>:8081/`) で、720p と 1080p (どちらも 30fps) を切り替えられる。API は `POST /api/size` (size=1280x720 など) と `GET /api/status`。切り替えは約 0.15 秒で、配信中のクライアントは切れない。
- 取得でエラーが起きたとき (キャプチャボードを抜いたときなど) は、自動で開き直す。
- ファン制御の残り (`/root` の lichee-fans、pwmfan、lfant-*、gpio*、edge*、memtest、および `/etc/inittab` の lfan の行とそのバックアップ) は削除した。

### 音声 (2026-10-04 追加)

- HDMI の音声は、キャプチャボードの ALSA デバイス (`hw:CARD=MS2109,DEV=0`、S16LE、48kHz、ステレオ) から取れる。カーネルに MS2109 用の補正 (quirk) が入っている。Windows 側で、音声の出力先を HDMI にしておく必要がある。
- `kvmcap` は `/ws/audio` (WebSocket) で PCM をそのまま流す。最初のテキストメッセージがフォーマット情報の JSON で、以降は 20ms (3840 バイト) ごとのバイナリ。帯域は約 1.5Mbps。聞いている人がいるときだけ `arecord` を動かし、全員いなくなってから 3 秒後に止める。
- ブラウザでの再生は AudioBufferSource を順番に予約する方式。AudioWorklet は安全な接続 (HTTPS など) が必要で、http では使えないため。最初は 80ms ためてから鳴らし、300ms を超えてたまったら捨てて遅延を詰める。
- WebSocket は `ws.go` に自前で実装した (RFC 6455 の最小限)。入力 (キーボードとマウス) にも使う予定。
- 音声の取得 (snd-usb-audio) だけで、CPU を約 12% 使う。カーネル側の固定費で、period の長さを変えても減らない。
- 取得スレッドの優先度を nice -10 にした。CPU が足りないときに、取得ではなく配信が遅れるようにするため。映像と音声を同時に配信したときの結果は次のとおり (15 秒、1 クライアント)。
  - 720p: 取得 30fps、欠け 0、クライアントの受信 30fps、音声の欠け 0。
  - 1080p: 取得 30fps、欠け 1、クライアントの受信 約 27fps、音声の欠け 0。
- 起動時の解像度は 720p にした (S95kvmcap の `-size 1280x720`)。1080p は Web UI から選べる。

### 遅延の削減 (2026-10-04 追加)

- 映像の表示を `<img>` + multipart から、canvas + WebSocket (`/ws/video`) に変えた。ブラウザは描画するたびに ack を返し、サーバーは未 ack のフレームを最大 2 枚までに抑えて、常に最新のフレームを送る。multipart には、ブラウザが遅れたときに送信を抑える仕組み (back-pressure) がない。そのため、TCP の送受信バッファ (数 MB) にフレームが溜まり、遅延が伸びていたと考えられる。
- フレームには、取得した時刻 (サーバーの時計、ms) を 8 バイトの先頭に付けている。ブラウザは ping/pong で時計を合わせ、「映像遅延」として、USB でフレームを受け終わってから描画するまでの時間を表示する。MS2109 の内部遅延と、ディスプレイの表示遅延は含まない。
- 測定 (720p、LAN、Python のクライアントで受信時点まで): 中央値 24ms、p90 31ms、最大 63ms。1 枚あたり 50ms かかる遅いクライアントでも、遅延は約 50ms のままで、フレームが間引かれる (溜まらない)。
- `/stream` (multipart) は互換のために残した。区切りをフレームの直後に送るように直し、1 フレーム分の待ちをなくした。
- 音声: サーバーのクライアントごとの待ち行列を 500ms から 100ms にした (溢れたら古いものを捨てる)。ブラウザ側は、最初に 60ms ためる。目標より 40ms 以上多くたまった状態が 1 秒続いたら、20ms 分を捨てて戻す。上限は 200ms。

### 残課題

- 再起動後に、host モードと kvmcap が自動で上がるかは未確認。
- 1080p30 で 1 クライアントに配信すると、CPU の余りがほぼない。入力処理 (UART、WebSocket) を入れたときに映像が欠けないかを確認する。欠ける場合は、配信の間引きか 720p を検討する。
- 次は ESP32-S3 の HID (`kvm-design.md` の実装順序 1〜3)。その後、Web UI からのキーボードとマウス操作を kvmcap に統合する。

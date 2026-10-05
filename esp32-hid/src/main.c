// IP-KVM HID bridge for ESP32-S3.
//
// The native USB port (USB-OTG) appears to the target PC as a composite
// device with three HID interfaces:
//   0: boot-protocol keyboard
//   1: boot-protocol relative mouse (works in BIOS/GRUB)
//   2: absolute mouse, X/Y in 0..32767 (OS only; BIOS ignores it)
// Commands arrive as ASCII lines on UART0 (the CH343 USB-serial port,
// 115200 8N1), which also carries the ESP-IDF console/log output.
//
// Commands (one per line; numbers are decimal or 0x-prefixed hex):
//   KD <usage>     key down (HID Keyboard/Keypad page usage ID)  -> OK | ERR <reason>
//   KU <usage>     key up                                        -> OK | ERR <reason>
//   KR             release all keys                              -> OK
//   MA <x> <y>     absolute move, 0..32767                       -> OK
//   MR <dx> <dy>   relative move (split into steps of <=127)     -> OK
//   MB <mask>      buttons: bit0 left, bit1 right, bit2 middle, bit3 back, bit4 forward -> OK
//   MW <d>         wheel, positive = up, -127..127               -> OK
//   ST             status -> ST mounted=<0|1> suspended=<0|1> protocol=<boot|report> leds=<hex>
//   PING           -> PONG
//   VER            -> VER <name> <version>
// MB and MW go to the mouse interface used last (MA: absolute, MR: relative;
// absolute before any move).
// Unsolicited:
//   LED <hex>      keyboard LEDs set by the target (bit0 NumLock, bit1 CapsLock, bit2 ScrollLock)
//   READY <name> <version>   after boot
//
// The host should only parse lines starting with these keywords; anything
// else (boot ROM messages, logs) is noise.

#include <ctype.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "driver/uart.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"
#include "freertos/task.h"
#include "tinyusb.h"
#include "tinyusb_default_config.h"
#include "class/hid/hid_device.h"

#define FW_NAME    "kvm-hid"
#define FW_VERSION "0.2.1"

#define CMD_UART      UART_NUM_0
#define CMD_LINE_MAX  64
#define REPORT_QUEUE  128

static const char *TAG = "kvm-hid";

// ---------------------------------------------------------------------------
// USB descriptors

enum {
    ITF_KEYBOARD = 0,
    ITF_MOUSE,
    ITF_ABSMOUSE,
    ITF_TOTAL,
};

#define EP_KEYBOARD_IN 0x81
#define EP_MOUSE_IN    0x82
#define EP_ABSMOUSE_IN 0x83

static const uint8_t keyboard_report_desc[] = {TUD_HID_REPORT_DESC_KEYBOARD()};
static const uint8_t mouse_report_desc[] = {TUD_HID_REPORT_DESC_MOUSE()};
static const uint8_t absmouse_report_desc[] = {TUD_HID_REPORT_DESC_ABSMOUSE()};

static const tusb_desc_device_t device_desc = {
    .bLength = sizeof(tusb_desc_device_t),
    .bDescriptorType = TUSB_DESC_DEVICE,
    .bcdUSB = 0x0200,
    .bDeviceClass = 0x00, // defined per interface
    .bDeviceSubClass = 0x00,
    .bDeviceProtocol = 0x00,
    .bMaxPacketSize0 = CFG_TUD_ENDPOINT0_SIZE,
    .idVendor = 0x303A,   // Espressif; PID 0x4004 follows the TinyUSB example scheme (HID)
    .idProduct = 0x4004,
    .bcdDevice = 0x0200,  // bumped when the interface set changes
    .iManufacturer = 1,
    .iProduct = 2,
    .iSerialNumber = 3,
    .bNumConfigurations = 1,
};

#define CONFIG_TOTAL_LEN (TUD_CONFIG_DESC_LEN + 3 * TUD_HID_DESC_LEN)

static const uint8_t config_desc[] = {
    TUD_CONFIG_DESCRIPTOR(1, ITF_TOTAL, 0, CONFIG_TOTAL_LEN, TUSB_DESC_CONFIG_ATT_REMOTE_WAKEUP, 100),
    // All interfaces polled every 1 ms.
    TUD_HID_DESCRIPTOR(ITF_KEYBOARD, 4, HID_ITF_PROTOCOL_KEYBOARD, sizeof(keyboard_report_desc),
                       EP_KEYBOARD_IN, 8, 1),
    TUD_HID_DESCRIPTOR(ITF_MOUSE, 5, HID_ITF_PROTOCOL_MOUSE, sizeof(mouse_report_desc),
                       EP_MOUSE_IN, 8, 1),
    TUD_HID_DESCRIPTOR(ITF_ABSMOUSE, 6, HID_ITF_PROTOCOL_NONE, sizeof(absmouse_report_desc),
                       EP_ABSMOUSE_IN, 8, 1),
};

static const char *string_desc[] = {
    (const char[]){0x09, 0x04}, // 0: English (0x0409)
    "ip-kvm",                   // 1: manufacturer
    "KVM HID",                  // 2: product
    "000001",                   // 3: serial
    "KVM Keyboard",             // 4
    "KVM Mouse",                // 5
    "KVM Absolute Mouse",       // 6
};

uint8_t const *tud_hid_descriptor_report_cb(uint8_t instance)
{
    switch (instance) {
    case ITF_MOUSE:
        return mouse_report_desc;
    case ITF_ABSMOUSE:
        return absmouse_report_desc;
    default:
        return keyboard_report_desc;
    }
}

uint16_t tud_hid_get_report_cb(uint8_t instance, uint8_t report_id, hid_report_type_t report_type,
                               uint8_t *buffer, uint16_t reqlen)
{
    (void)instance; (void)report_id; (void)report_type; (void)buffer; (void)reqlen;
    return 0;
}

static volatile uint8_t led_state;

static void cmd_write(const char *s);

// Output report from the host: keyboard LEDs.
void tud_hid_set_report_cb(uint8_t instance, uint8_t report_id, hid_report_type_t report_type,
                           uint8_t const *buffer, uint16_t bufsize)
{
    (void)report_id;
    if (instance == ITF_KEYBOARD && report_type == HID_REPORT_TYPE_OUTPUT && bufsize >= 1 &&
        buffer[0] != led_state) {
        led_state = buffer[0];
        char line[16];
        snprintf(line, sizeof line, "LED %02x\n", led_state);
        cmd_write(line);
    }
}

// ---------------------------------------------------------------------------
// Report queue: every state change is queued (in order, across interfaces)
// so a quick press+release is never merged away before the host polls.

typedef struct {
    uint8_t itf;
    uint8_t len;
    uint8_t data[8];
} report_t;

static QueueHandle_t report_queue;

static void push_report(uint8_t itf, const void *data, uint8_t len)
{
    report_t r = {.itf = itf, .len = len};
    memcpy(r.data, data, len);
    if (xQueueSend(report_queue, &r, 0) != pdTRUE) {
        ESP_LOGW(TAG, "report queue full");
    }
}

static void usb_send_task(void *arg)
{
    (void)arg;
    report_t r;
    for (;;) {
        xQueueReceive(report_queue, &r, portMAX_DELAY);
        if (tud_suspended()) {
            // Wake the target if it allowed remote wakeup (e.g. from sleep).
            tud_remote_wakeup();
        }
        // Wait for the previous report on this interface to be taken.
        for (int waited = 0; !tud_hid_n_ready(r.itf); waited++) {
            if (!tud_mounted() || waited > 500) {
                break; // not connected: drop the report
            }
            vTaskDelay(pdMS_TO_TICKS(1));
        }
        if (!tud_hid_n_ready(r.itf)) {
            continue;
        }
        uint8_t len = r.len;
        if (r.itf == ITF_MOUSE && tud_hid_n_get_protocol(ITF_MOUSE) == HID_PROTOCOL_BOOT) {
            len = 3; // boot mouse report: buttons, x, y
        }
        tud_hid_n_report(r.itf, 0, r.data, len);
    }
}

// ---------------------------------------------------------------------------
// Keyboard state (owned by the command task)

typedef struct {
    uint8_t modifier;
    uint8_t reserved;
    uint8_t keys[6];
} kbd_report_t;

static kbd_report_t kbd;

static void kbd_push(void) { push_report(ITF_KEYBOARD, &kbd, sizeof kbd); }

static const char *kbd_down(uint8_t usage)
{
    if (usage >= HID_KEY_CONTROL_LEFT && usage <= HID_KEY_GUI_RIGHT) {
        kbd.modifier |= 1 << (usage - HID_KEY_CONTROL_LEFT);
        kbd_push();
        return NULL;
    }
    if (usage == HID_KEY_NONE) {
        return "invalid usage";
    }
    for (int i = 0; i < 6; i++) {
        if (kbd.keys[i] == usage) {
            return NULL; // already down
        }
    }
    for (int i = 0; i < 6; i++) {
        if (kbd.keys[i] == 0) {
            kbd.keys[i] = usage;
            kbd_push();
            return NULL;
        }
    }
    return "rollover (6 keys already down)";
}

static void kbd_up(uint8_t usage)
{
    if (usage >= HID_KEY_CONTROL_LEFT && usage <= HID_KEY_GUI_RIGHT) {
        kbd.modifier &= ~(1 << (usage - HID_KEY_CONTROL_LEFT));
        kbd_push();
        return;
    }
    for (int i = 0; i < 6; i++) {
        if (kbd.keys[i] == usage) {
            // Keep the array packed so slots stay in press order.
            memmove(&kbd.keys[i], &kbd.keys[i + 1], 5 - i);
            kbd.keys[5] = 0;
            kbd_push();
            return;
        }
    }
}

static void kbd_release_all(void)
{
    memset(&kbd, 0, sizeof kbd);
    kbd_push();
}

// ---------------------------------------------------------------------------
// Mouse state (owned by the command task)

static uint8_t mouse_buttons;
static uint16_t abs_x = 16384, abs_y = 16384;
static bool mouse_abs = true; // interface MB/MW go to
// Buttons last reported on each interface. The target keeps a separate
// button state per device, so switching interfaces must release the old one.
static uint8_t rel_sent_buttons, abs_sent_buttons;

static void abs_push(int8_t wheel);

static void rel_push(int8_t dx, int8_t dy, int8_t wheel)
{
    if (abs_sent_buttons) {
        uint8_t b = mouse_buttons;
        mouse_buttons = 0;
        abs_push(0);
        mouse_buttons = b;
    }
    uint8_t r[5] = {mouse_buttons, (uint8_t)dx, (uint8_t)dy, (uint8_t)wheel, 0};
    push_report(ITF_MOUSE, r, sizeof r);
    rel_sent_buttons = mouse_buttons;
}

static void abs_push(int8_t wheel)
{
    if (rel_sent_buttons) {
        uint8_t b = mouse_buttons;
        mouse_buttons = 0;
        rel_push(0, 0, 0);
        mouse_buttons = b;
    }
    uint8_t r[7] = {mouse_buttons, abs_x & 0xff, abs_x >> 8, abs_y & 0xff, abs_y >> 8, (uint8_t)wheel, 0};
    push_report(ITF_ABSMOUSE, r, sizeof r);
    abs_sent_buttons = mouse_buttons;
}

static int clamp(long v, int lo, int hi) { return v < lo ? lo : v > hi ? hi : (int)v; }

static void mouse_move_rel(long dx, long dy)
{
    mouse_abs = false;
    do {
        int sx = clamp(dx, -127, 127), sy = clamp(dy, -127, 127);
        rel_push(sx, sy, 0);
        dx -= sx;
        dy -= sy;
    } while (dx != 0 || dy != 0);
}

static void mouse_move_abs(long x, long y)
{
    mouse_abs = true;
    abs_x = clamp(x, 0, 32767);
    abs_y = clamp(y, 0, 32767);
    abs_push(0);
}

static void mouse_set_buttons(uint8_t mask)
{
    mouse_buttons = mask & 0x1f;
    if (mouse_abs) {
        abs_push(0);
    } else {
        rel_push(0, 0, 0);
    }
}

static void mouse_wheel(long d)
{
    int w = clamp(d, -127, 127);
    if (mouse_abs) {
        abs_push(w);
    } else {
        rel_push(0, 0, w);
    }
}

// ---------------------------------------------------------------------------
// UART command interface

static void cmd_write(const char *s)
{
    uart_write_bytes(CMD_UART, s, strlen(s));
}

// Parses up to n integers separated by spaces; returns how many were found,
// or -1 on junk.
static int parse_ints(const char *s, long *out, int n)
{
    int count = 0;
    while (s && *s) {
        while (*s == ' ') s++;
        if (!*s) break;
        if (count == n) return -1;
        char *end;
        long v = strtol(s, &end, 0);
        if (end == s || (*end != ' ' && *end != '\0')) return -1;
        out[count++] = v;
        s = end;
    }
    return count;
}

static void handle_line(char *line)
{
    char *arg = strchr(line, ' ');
    if (arg) {
        *arg++ = '\0';
    }
    char resp[96];
    long v[2];
    int n = parse_ints(arg, v, 2);

    if (strcmp(line, "KD") == 0 || strcmp(line, "KU") == 0) {
        if (n != 1 || v[0] < 0 || v[0] > 0xff) {
            cmd_write("ERR bad usage\n");
            return;
        }
        const char *err = NULL;
        if (line[1] == 'D') {
            err = kbd_down((uint8_t)v[0]);
        } else {
            kbd_up((uint8_t)v[0]);
        }
        if (err) {
            snprintf(resp, sizeof resp, "ERR %s\n", err);
            cmd_write(resp);
        } else {
            cmd_write("OK\n");
        }
    } else if (strcmp(line, "KR") == 0) {
        kbd_release_all();
        cmd_write("OK\n");
    } else if (strcmp(line, "MA") == 0 || strcmp(line, "MR") == 0) {
        if (n != 2) {
            cmd_write("ERR expected 2 numbers\n");
            return;
        }
        if (line[1] == 'A') {
            mouse_move_abs(v[0], v[1]);
        } else {
            mouse_move_rel(clamp(v[0], -4096, 4096), clamp(v[1], -4096, 4096));
        }
        cmd_write("OK\n");
    } else if (strcmp(line, "MB") == 0) {
        if (n != 1 || v[0] < 0 || v[0] > 0x1f) {
            cmd_write("ERR bad button mask\n");
            return;
        }
        mouse_set_buttons((uint8_t)v[0]);
        cmd_write("OK\n");
    } else if (strcmp(line, "MW") == 0) {
        if (n != 1) {
            cmd_write("ERR expected 1 number\n");
            return;
        }
        mouse_wheel(v[0]);
        cmd_write("OK\n");
    } else if (strcmp(line, "ST") == 0) {
        snprintf(resp, sizeof resp, "ST mounted=%d suspended=%d protocol=%s leds=%02x\n",
                 tud_mounted(), tud_suspended(),
                 tud_hid_n_get_protocol(ITF_KEYBOARD) == HID_PROTOCOL_BOOT ? "boot" : "report",
                 led_state);
        cmd_write(resp);
    } else if (strcmp(line, "PING") == 0) {
        cmd_write("PONG\n");
    } else if (strcmp(line, "VER") == 0) {
        cmd_write("VER " FW_NAME " " FW_VERSION "\n");
    } else if (line[0] != '\0') {
        cmd_write("ERR unknown command\n");
    }
}

static void cmd_task(void *arg)
{
    (void)arg;
    char line[CMD_LINE_MAX];
    size_t len = 0;
    bool overflow = false;
    uint8_t buf[64];
    for (;;) {
        // Block for the first byte, then take whatever else has arrived
        // (uart_read_bytes waits until the full length is read otherwise).
        int n = uart_read_bytes(CMD_UART, buf, 1, portMAX_DELAY);
        size_t avail = 0;
        uart_get_buffered_data_len(CMD_UART, &avail);
        if (n == 1 && avail > 0) {
            n += uart_read_bytes(CMD_UART, buf + 1, avail < sizeof buf - 1 ? avail : sizeof buf - 1, 0);
        }
        for (int i = 0; i < n; i++) {
            char c = (char)buf[i];
            if (c == '\r') {
                continue;
            }
            if (c == '\n') {
                line[len] = '\0';
                if (overflow) {
                    cmd_write("ERR line too long\n");
                } else {
                    handle_line(line);
                }
                len = 0;
                overflow = false;
            } else if (len < sizeof line - 1) {
                line[len++] = (char)toupper((unsigned char)c);
            } else {
                overflow = true;
            }
        }
    }
}

// ---------------------------------------------------------------------------

static void usb_event_cb(tinyusb_event_t *event, void *arg)
{
    (void)arg;
    switch (event->id) {
    case TINYUSB_EVENT_ATTACHED:
        ESP_LOGI(TAG, "USB attached");
        break;
    case TINYUSB_EVENT_DETACHED:
        ESP_LOGI(TAG, "USB detached");
        break;
    default:
        break;
    }
}

void app_main(void)
{
    report_queue = xQueueCreate(REPORT_QUEUE, sizeof(report_t));

    ESP_ERROR_CHECK(uart_driver_install(CMD_UART, 1024, 1024, 0, NULL, 0));

    tinyusb_config_t cfg = TINYUSB_DEFAULT_CONFIG(usb_event_cb);
    cfg.descriptor.device = &device_desc;
    cfg.descriptor.string = string_desc;
    cfg.descriptor.string_count = sizeof(string_desc) / sizeof(string_desc[0]);
    cfg.descriptor.full_speed_config = config_desc;
    ESP_ERROR_CHECK(tinyusb_driver_install(&cfg));

    xTaskCreate(usb_send_task, "usb_send", 4096, NULL, 6, NULL);
    xTaskCreate(cmd_task, "cmd", 4096, NULL, 5, NULL);

    cmd_write("READY " FW_NAME " " FW_VERSION "\n");
}

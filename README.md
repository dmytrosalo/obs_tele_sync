# Obsidian Telegram Bot (Go)

Telegram → Google Drive → Obsidian vault.
Пересилаєш повідомлення боту з телефону — воно з'являється в vault.

## Що підтримує

- Текст (і forwarded з інших чатів)
- Фото
- Файли (PDF, DOCX, тощо)
- Голосові та відеоповідомлення

## Налаштування

### 1. Telegram бот

```
@BotFather → /newbot → отримай токен
@userinfobot → дізнайся свій user ID
```

### 2. Google Service Account

1. [console.cloud.google.com](https://console.cloud.google.com/) → створи проект
2. APIs & Services → Enable **Google Drive API**
3. Credentials → Create → Service Account → завантаж JSON ключ
4. Скопіюй email service account
5. В Google Drive: ПКМ на папку `dvygar` → Share → додай email як Editor
6. Скопіюй ID папки з URL

### 3. Деплой на DO дроплет

```bash
ssh root@your-droplet

# Клонуй проект
git clone <repo> /opt/obsidian-tg-bot
cd /opt/obsidian-tg-bot

# Скопіюй service_account.json
# scp service_account.json root@droplet:/opt/obsidian-tg-bot/

# Конфіг
cp .env.example .env
nano .env

# Збери та запусти
go build -o bot .
./bot
```

### 4. Systemd (автозапуск)

```bash
cat > /etc/systemd/system/obsidian-tg-bot.service << 'EOF'
[Unit]
Description=Obsidian Telegram Bot
After=network.target

[Service]
Type=simple
WorkingDirectory=/opt/obsidian-tg-bot
ExecStart=/opt/obsidian-tg-bot/bot
Restart=always
RestartSec=10
EnvironmentFile=/opt/obsidian-tg-bot/.env

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now obsidian-tg-bot
```

## Структура в vault

```
dvygar/
├── inbox/              ← нотатки від бота
│   ├── 2026-04-14_22-45-01_text.md
│   └── 2026-04-14_22-46-30_photo.md
└── attachments/        ← медіа файли
    ├── 2026-04-14_22-46-30_photo.jpg
    └── 2026-04-14_22-50-00_voice.ogg
```

## Логи

```bash
journalctl -u obsidian-tg-bot -f
```

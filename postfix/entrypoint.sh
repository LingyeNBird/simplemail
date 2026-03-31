#!/bin/bash
set -e

# ============================================================
# TempMail 单容器入口脚本
# - 配置 Postfix
# - 创建域名同步脚本
# - 启动 supervisord（管理 api-server + postfix + sync-domains）
# ============================================================

echo "==> Setting up Postfix..."

chmod +x /usr/local/bin/mail-receiver

# 生成初始虚拟域名列表
echo "${SMTP_HOSTNAME:-mail.example.com}     OK" > /etc/postfix/virtual_domains

# 创建域名同步脚本（从本地 Go API 拉取域名列表）
cat > /usr/local/bin/sync-domains.sh << 'SCRIPT'
#!/bin/bash
while true; do
    DOMAINS=$(curl -sf http://localhost:8081/internal/domains 2>/dev/null || echo "")
    if [ -n "$DOMAINS" ]; then
        echo "$DOMAINS" | python3 -c "
import sys, json
data = json.load(sys.stdin)
for d in data.get('domains', []):
    if d.get('is_active', False):
        print(f\"{d['domain']}     OK\")
" > /etc/postfix/virtual_domains.new
        if [ -s /etc/postfix/virtual_domains.new ]; then
            mv /etc/postfix/virtual_domains.new /etc/postfix/virtual_domains
            postmap /etc/postfix/virtual_domains
            postfix reload 2>/dev/null || true
        fi
    fi
    sleep 60
done
SCRIPT
chmod +x /usr/local/bin/sync-domains.sh

# 初始化 postmap
postmap /etc/postfix/virtual_domains

# 配置 Postfix
postconf -e "myhostname=${SMTP_HOSTNAME:-mail.example.com}"
postconf -e "virtual_mailbox_domains=hash:/etc/postfix/virtual_domains"
postconf -e "virtual_transport=mailreceiver:"

echo "==> Starting services via supervisord..."
exec /usr/bin/supervisord -c /etc/supervisor/conf.d/supervisord.conf

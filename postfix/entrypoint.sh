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
# - hash 表：is_active 域名（原有白名单，精确匹配）
# - regexp 表：管理员配置的 retained_domain_patterns（补充收信面，用于留存邮件）
cat > /usr/local/bin/sync-domains.sh << 'SCRIPT'
#!/bin/bash
while true; do
    RESP=$(curl -sf http://localhost:8081/internal/virtual-domains 2>/dev/null || echo "")
    if [ -n "$RESP" ]; then
        # 写 hash 表（active 域名）
        echo "$RESP" | python3 -c "
import sys, json
data = json.load(sys.stdin)
for d in data.get('hash_domains', []):
    print(f\"{d}     OK\")
" > /etc/postfix/virtual_domains.new
        # 写 regexp 表（留存规则）
        echo "$RESP" | python3 -c "
import sys, json
data = json.load(sys.stdin)
for line in data.get('regexp_lines', []):
    print(line)
" > /etc/postfix/virtual_domains_regexp.new

        if [ -s /etc/postfix/virtual_domains.new ]; then
            mv /etc/postfix/virtual_domains.new /etc/postfix/virtual_domains
            postmap /etc/postfix/virtual_domains
        fi
        # regexp 表无条件覆盖——用户清空规则时应生成空文件
        mv /etc/postfix/virtual_domains_regexp.new /etc/postfix/virtual_domains_regexp
        postfix reload 2>/dev/null || true
    fi
    sleep 60
done
SCRIPT
chmod +x /usr/local/bin/sync-domains.sh

# 初始化 postmap
postmap /etc/postfix/virtual_domains
# 初始化空的 regexp 表（如果不存在），避免 Postfix 启动报错
touch /etc/postfix/virtual_domains_regexp

# 配置 Postfix
postconf -e "myhostname=${SMTP_HOSTNAME:-mail.example.com}"
postconf -e "virtual_mailbox_domains=hash:/etc/postfix/virtual_domains,regexp:/etc/postfix/virtual_domains_regexp"
postconf -e "virtual_transport=mailreceiver:"

echo "==> Starting services via supervisord..."
exec /usr/bin/supervisord -c /etc/supervisor/conf.d/supervisord.conf

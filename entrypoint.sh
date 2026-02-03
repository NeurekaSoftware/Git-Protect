#!/bin/sh
set -e

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"
TZ="${TZ:-UTC}"

if [ -n "${TZ}" ] && [ -f "/usr/share/zoneinfo/${TZ}" ]; then
  ln -snf "/usr/share/zoneinfo/${TZ}" /etc/localtime
  echo "${TZ}" > /etc/timezone
fi

APP_GROUP="appgroup"
APP_USER="appuser"

if ! getent group "${APP_GROUP}" >/dev/null 2>&1; then
  if ! groupadd -g "${PGID}" "${APP_GROUP}" 2>/dev/null; then
    groupadd "${APP_GROUP}"
  fi
else
  EXISTING_GID="$(getent group "${APP_GROUP}" | cut -d: -f3)"
  if [ "${EXISTING_GID}" != "${PGID}" ]; then
    groupmod -o -g "${PGID}" "${APP_GROUP}" || true
  fi
fi

if ! id -u "${APP_USER}" >/dev/null 2>&1; then
  if ! useradd -u "${PUID}" -g "${PGID}" -s /bin/sh -M "${APP_USER}" 2>/dev/null; then
    useradd -g "${PGID}" -s /bin/sh -M "${APP_USER}"
  fi
else
  EXISTING_UID="$(id -u "${APP_USER}")"
  if [ "${EXISTING_UID}" != "${PUID}" ]; then
    usermod -o -u "${PUID}" -g "${PGID}" "${APP_USER}" || true
  fi
fi

mkdir -p /app /data
chown -R "${PUID}:${PGID}" /app /data
chmod 755 /app
chmod 775 /data

exec su -s /bin/sh -c "cd /app && dotnet GitProtect.dll" "${APP_USER}"

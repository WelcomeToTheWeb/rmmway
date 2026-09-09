-- Migration 0013: Deep inventory storage (gap #4)
-- Stores device hardware, software, services, and user account inventory.

-- Hardware inventory (one row per device, updated on each inventory collection)
CREATE TABLE IF NOT EXISTS device_hardware (
    device_id TEXT PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    cpu_model TEXT,
    cpu_vendor TEXT,
    cpu_cores INTEGER,
    cpu_logical INTEGER,
    ram_total_bytes BIGINT,
    os_name TEXT,
    os_version TEXT,
    os_arch TEXT,
    hostname TEXT,
    collected_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_device_hardware_hostname ON device_hardware(hostname);

-- Installed software inventory (one row per package per device)
CREATE TABLE IF NOT EXISTS device_software (
    device_id TEXT REFERENCES devices(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    version TEXT,
    vendor TEXT,
    install_date TEXT,
    arch TEXT,
    source TEXT,
    collected_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (device_id, name)
);

CREATE INDEX IF NOT EXISTS idx_device_software_name ON device_software(name);
CREATE INDEX IF NOT EXISTS idx_device_software_version ON device_software(name, version);

-- System services inventory (one row per service per device)
CREATE TABLE IF NOT EXISTS device_services (
    device_id TEXT REFERENCES devices(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    display_name TEXT,
    status TEXT,
    service_type TEXT,
    enabled BOOLEAN,
    collected_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (device_id, name)
);

CREATE INDEX IF NOT EXISTS idx_device_services_status ON device_services(status);

-- User accounts inventory (one row per account per device)
CREATE TABLE IF NOT EXISTS device_users (
    device_id TEXT REFERENCES devices(id) ON DELETE CASCADE,
    username TEXT NOT NULL,
    uid TEXT,
    home_dir TEXT,
    shell TEXT,
    enabled BOOLEAN,
    account_type TEXT,
    collected_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (device_id, username)
);

-- Patch inventory (available and installed patches per device)
CREATE TABLE IF NOT EXISTS device_patches (
    device_id TEXT REFERENCES devices(id) ON DELETE CASCADE,
    patch_id TEXT NOT NULL,
    title TEXT,
    severity TEXT,
    kb_article TEXT,
    status TEXT NOT NULL, -- 'available' | 'installed' | 'failed'
    install_date TEXT,
    reboot_required BOOLEAN DEFAULT FALSE,
    collected_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (device_id, patch_id, status)
);

CREATE INDEX IF NOT EXISTS idx_device_patches_status ON device_patches(status);
CREATE INDEX IF NOT EXISTS idx_device_patches_severity ON device_patches(severity);
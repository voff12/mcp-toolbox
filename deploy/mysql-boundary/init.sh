#!/bin/bash
# Creates the fake data and the only account Toolbox may use: read-only, limited
# to one schema, and accepted from the Toolbox address alone.
set -euo pipefail
root_password=$(cat /run/secrets/mysql_root_password)
db_password=$(cat /run/secrets/db_password)

mysql -uroot -p"$root_password" <<SQL
CREATE DATABASE shop DEFAULT CHARSET utf8mb4;
USE shop;
CREATE TABLE customers (
  id INT PRIMARY KEY AUTO_INCREMENT,
  customer_name VARCHAR(50),
  phone VARCHAR(20),
  level VARCHAR(10)
);
INSERT INTO customers (customer_name, phone, level) VALUES
  ('张三丰', '13812345678', 'gold'),
  ('李四', '13987654321', 'gold');

CREATE USER 'toolbox_ro'@'172.30.1.10' IDENTIFIED BY '$db_password';
GRANT SELECT ON shop.* TO 'toolbox_ro'@'172.30.1.10';
GRANT SELECT ON performance_schema.events_statements_summary_by_digest TO 'toolbox_ro'@'172.30.1.10';

-- For verify.sh: identityCheck must refuse both of these.
CREATE USER 'toolbox_rw'@'172.30.1.10' IDENTIFIED BY '$db_password';
GRANT SELECT, INSERT ON shop.* TO 'toolbox_rw'@'172.30.1.10';
CREATE USER 'toolbox_any'@'%' IDENTIFIED BY '$db_password';
GRANT SELECT ON shop.* TO 'toolbox_any'@'%';
SQL

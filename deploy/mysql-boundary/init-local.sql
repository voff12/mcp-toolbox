-- Run as a MySQL administrator before starting Toolbox with the root tools.yaml.
-- Replace the password below before running this script in a shared environment.
CREATE DATABASE IF NOT EXISTS shop
  DEFAULT CHARACTER SET utf8mb4
  COLLATE utf8mb4_unicode_ci;

USE shop;

CREATE TABLE IF NOT EXISTS customers (
  id INT PRIMARY KEY AUTO_INCREMENT,
  customer_name VARCHAR(50) NOT NULL,
  phone VARCHAR(20) NOT NULL,
  level VARCHAR(10) NOT NULL
);

INSERT INTO customers (customer_name, phone, level)
SELECT * FROM (
  SELECT '张三丰' AS customer_name, '13812345678' AS phone, 'gold' AS level
  UNION ALL SELECT '李四', '13987654321', 'gold'
  UNION ALL SELECT '王五', '13711112222', 'silver'
  UNION ALL SELECT '赵六', '13633334444', 'bronze'
) AS sample_customers
WHERE NOT EXISTS (SELECT 1 FROM customers);

-- The host matches the source address observed by the local MySQL server.
CREATE USER IF NOT EXISTS 'toolbox_ro'@'192.168.65.1'
  IDENTIFIED BY 'ChangeMe_ToolboxRO_2026!';
ALTER USER 'toolbox_ro'@'192.168.65.1'
  IDENTIFIED BY 'ChangeMe_ToolboxRO_2026!';
GRANT SELECT ON shop.* TO 'toolbox_ro'@'192.168.65.1';

SHOW GRANTS FOR 'toolbox_ro'@'192.168.65.1';
SELECT id, customer_name, phone, level FROM customers ORDER BY id;

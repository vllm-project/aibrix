## Rules for validating SQL statements
- The table name length is not recommended to exceed 49.
- Do not use implicit conversion in SQL statements. For example, do not use strings as integers because indexes cannot be used.
- When you create a table or add a column field, you must add a comment to the table or column field. The following is a demo case. Pay special attention to the location of table and field comments.
```
create table tbl(
id bigint unsigned not null auto_increment primary key comment 'primary id',
user_name varchar(63) not null comment 'user name',
key idx_user_name(user_name)
)ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 comment='user information table';
```
- The prefix of an index name must be `idx_`, and the prefix of a unique constraint/index must be `uniq_`.
- Use single quotation marks.
- The maximum number of columns in a composite index must not exceed 5.
- The maximum number of indexes in a table must not exceed 16.
- It is not recommended to use CHAR or BINARY. We recommend that you use VARCHAR or VARBINARY instead.
- It is not recommended to use FLOAT, REAL, or DOUBLE. We recommend that you use DECIMAL instead.
- Do not use the ENUM data type because the table is locked when a new ENUM type is added. We recommend that you use varchar.
- It is not recommended to set the value of a BLOB or TEXT field to NOT NULL.
- Do not use a different character set for column fields and tables. Do not specify the character set of a column separately.
- Do not use a VARCHAR that is longer than 2048. For characters that are longer than 2048, we recommend that you use text or blob values.
- Do not use the SYSDATE() function as the default value of a field. Otherwise, data inconsistency may occur between the source and replica.
- Do not use triggers on tables.
- Do not use stored procedures.
- Do not create a table without a primary key or a unique key.
- Do not use recursive relationships such as foreign keys.
- The primary key of a table must be bigint unsigned.
- Do not use partitioned tables.
- Do not use views.
- Do not use temporary tables.
- Do not use storage engines other than InnoDB and RocksDB.
- Do not use DUAL as a table name.
- Do not use MySQL keywords as column names or table names.
- Do not set to not null without a default value; except for JSON, which does not allow setting not null and default values.
- Do not use tables beginning with “_” and “~”.
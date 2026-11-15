# GuriMoon setup guide

How to run your own EVE Online 2005 server on your own computer.
Time: about 30 minutes, mostly waiting for the database to load.

All commands go in **Command Prompt** (cmd). To open one in the right place:
open the GuriMoon folder in File Explorer, click the address bar, type `cmd`
and press Enter.

---

## 1. What you need

Install these four things. Normal Windows installers, defaults are fine.

| Program | Get it from | Notes |
|---|---|---|
| **Go** | https://go.dev/dl/ | Version 1.24 or newer. Builds the server. |
| **MariaDB** | https://mariadb.org/download/ | Version 11 or newer. Stores the game. It asks for a **root password**. Pick one and write it down. |
| **Git** | https://git-scm.com/download/win | Downloads this project. |
| **EVE client** | You supply it | Red Moon Rising, version 2.10, build 3913. Must reach its login screen. |

Close and reopen Command Prompt after installing so it finds the new programs.

**Make `mysql` work.** Paste this, but use your MariaDB version in the path
(look in `C:\Program Files\` for the exact folder name):

```
set PATH=%PATH%;C:\Program Files\MariaDB 11.4\bin
mysql --version
```

If it prints a version, you are good. This only lasts for that window. Paste
the `set PATH` line again in any new window where you need `mysql`.

---

## 2. Download GuriMoon

```
git clone https://github.com/SODANK/GuriMoon.git
cd GuriMoon
```

Stay in this folder for every step below.

---

## 3. Set up the database

**3.1 Create it.** This makes an empty database called `gurimoon_rmr` and a
login called `gurimoon` (password `gurimoon`). It asks for your root password.

```
mysql -uroot -p -e "CREATE DATABASE gurimoon_rmr CHARACTER SET utf8mb4; CREATE USER 'gurimoon'@'localhost' IDENTIFIED BY 'gurimoon'; GRANT ALL ON gurimoon_rmr.* TO 'gurimoon'@'localhost';"
```

**3.2 Fill it with the game universe.** Takes a few minutes. It prints each
file name as it goes.

```
for /f "delims=" %f in ('dir /b /on sql\*.sql') do @echo %f & mysql -ugurimoon -pgurimoon gurimoon_rmr < "sql\%f"
```

(In a `.bat` file, write `%%f` instead of `%f`.)

**3.3 Check it.** You should see three numbers, all above zero.

```
mysql -ugurimoon -pgurimoon gurimoon_rmr -e "SELECT COUNT(*) FROM invtypes; SELECT COUNT(*) FROM mapsolarsystems; SELECT COUNT(*) FROM billtypes;"
```

If you saw an error or a zero, wipe it and redo 3.1 and 3.2:

```
mysql -uroot -p -e "DROP DATABASE gurimoon_rmr;"
```

---

## 4. Settings file

```
copy config.example.json config.json
```

Nothing in it needs changing for a server on your own computer. You can open
it in Notepad to look. Don't add settings of your own. The server refuses
settings it doesn't know.

---

## 5. Build and start the server

Build (about a minute the first time):

```
rebuild.bat
```

`rebuild.bat` compiles in every module found in the `modules` folder (for
example a module repo you dropped there) and lists them as it builds:

```
=== module GM-Rolebots : collecting overlay ===
=== Building server WITH modules: GM-Rolebots ===
=== Build OK: C:\GuriMoon\gurimoon.exe (57.1 MB) ===
```

With no modules it says `Building server (no modules found)`. To leave one
module out, put an empty file named `.disabled` in its folder.
`rebuild.bat core` builds without any module. `rebuild.bat check` does a test
compile into `build\` and leaves `gurimoon.exe` alone.

Start:

```
gurimoon.exe
```

Always start it from inside the GuriMoon folder. The first start takes a few
extra seconds. It is ready when you see:

```
GuriMoon listening on 127.0.0.1:26000
```

Leave that window open. Closing it stops the server. To stop it properly,
press **Ctrl+C**.

---

## 6. Connect the client

1. Start the EVE client. If it complains about updating or its version, ignore it.
2. In the **Server** box on the login screen, type `127.0.0.1`.
3. Type any username and password. The first login creates the account.
   Don't reuse a password you care about.
4. Connect, create a character, undock.

---

## Starting the server again later

1. Open Command Prompt in the GuriMoon folder.
2. Run:

   ```
   gurimoon.exe
   ```

3. Wait for the `listening` line, then start the client.

To stop: **Ctrl+C** in the server window.

**Updating to a new version** (server stopped first):

```
git pull
rebuild.bat
gurimoon.exe
```

If the update notes say a file in `sql\` changed, load that one file:

```
mysql -ugurimoon -pgurimoon gurimoon_rmr < sql\thatfile.sql
```

**Backup** (everything is in the database):

```
mysqldump -ugurimoon -pgurimoon gurimoon_rmr > backup.sql
```

**Where things are:** settings in `config.json`, logs in `server.log` and
`server.crash.log`, admin panel key and uploaded images in `data\`.

---

## Optional extras

### Admin panel and GM commands

Every account starts as a normal player. Make yours an admin (put your login
name in place of `yourname`):

```
mysql -ugurimoon -pgurimoon gurimoon_rmr -e "UPDATE account SET role = role | 56 WHERE accountName = 'yourname';"
```

Log out of the client and back in. Open http://127.0.0.1:26080/login in a web
browser and sign in with the same name and password. That's the admin panel
(see the ESP section of the README). In game you can now type `/` commands in chat
(see [docs/slash-commands.txt](docs/slash-commands.txt)).

### In-game browser and news

The client fetches news and its built-in web pages from two old CCP web
addresses. To point them at your server, open Notepad **as administrator**
(right-click Notepad, Run as administrator), open
`C:\Windows\System32\drivers\etc\hosts`, add these two lines at the bottom
and save:

```
127.0.0.1 myeve.eve-online.com
127.0.0.1 img.eve.is
```

Restart the client. Without this the game still works, the news panel is just
empty.

### Station sounds and small client patches

Stations are silent in the old client. This fixes it using sounds the client
already has. First set `"clientPath"` in `config.json` to the folder with
`eve.exe` in it (double the backslashes: `"D:\\Games\\EVE"`), or add
`-client "D:\Games\EVE"` to the commands. Run each without `-apply` to preview.

```
go run ./cmd/audiorestore -apply
go run ./cmd/clientpatch -apply
```

Undo the patches with `go run ./cmd/clientpatch -uninstall -apply`. Undo the
sounds by deleting the `res\Sound` folder it made inside the client.


### Docker instead of steps 1, 3, 4 and 5

With Docker Desktop installed, this replaces installing Go and MariaDB and
loading the database:

```
git clone https://github.com/SODANK/GuriMoon.git
cd GuriMoon
docker compose up -d
docker compose logs -f
```

The first start takes several minutes. Then carry on from step 6.

- Settings are in `config.docker.json`. After editing:
  `docker compose restart gurimoon`.
- If the database load fails, fix the cause, then `docker compose down -v` and
  `docker compose up -d`.
- The admin command becomes:
  `docker compose exec mariadb mariadb -ugurimoon -pgurimoon gurimoon_rmr -e "UPDATE account SET role = role | 56 WHERE accountName = 'yourname';"`
- For friends, remove the `127.0.0.1:` prefix from the `26000` and `80` lines
  in `docker-compose.yml`.

Linux without Docker: same steps, with `./rebuild.sh`. MariaDB needs
`lower_case_table_names = 1` under `[mysqld]` in its config, or the server
can't find its tables.

---

## Troubleshooting

| What you see | What to do |
|---|---|
| `load string table "data/strings.txt"`, server quits | You're in the wrong folder. `cd` into GuriMoon. |
| `database.host is required` | No `config.json`. Do step 4. |
| `'mysql' is not recognized` | Paste the `set PATH` line from step 1 again. |
| `Access denied for user 'gurimoon'` | Do step 3.1. |
| `listen 127.0.0.1:26000 ... in use` | An old server window is still open. Close it. |
| `billtypes table is empty` (or other empty tables) | Step 3.2 didn't finish. Check with 3.3. |
| Port 80 error at start | Something else uses port 80. Find it with `netstat -ano \| findstr ":80 "`. Stop it, or set `"newsListen": ""` in `config.json`. The game still works. |
| Client: unable to connect | Is the server window showing `listening`? Did you type `127.0.0.1`? For friends, see "Playing with friends". |
| Client: login refused | That name exists with another password. Use a new name. |
| Admin panel refuses your login | Make yourself admin (see above), then log in again. |
| Admin panel has no portraits or icons | Set `clientPath` in `config.json`. Cosmetic only. |
| Empty news panel, blank billboards | Add the hosts lines, and check port 80. |
| Stations are silent | Run the sound fix above. |
| Linux: `table ... doesn't exist` | See the Linux note under Docker. |

---

## Playing with friends

1. In `config.json` change `"listen": "127.0.0.1:26000"` to
   `"listen": "0.0.0.0:26000"`. Do the same for `"newsListen"`. Leave the
   admin panel line alone.
2. Allow the server through the firewall. Run this in a Command Prompt opened
   **as administrator**, with your real path:

   ```
   netsh advfirewall firewall add rule name="GuriMoon" dir=in action=allow program="C:\Games\GuriMoon\gurimoon.exe" protocol=TCP localport=26000,80
   ```

3. Restart the server.
4. Friends type **your** IP in the client's Server box. On the same home
   network, find it with `ipconfig` (IPv4 Address). Over the internet, use
   your public IP and forward port 26000 on your router. For the in-game
   browser they also add the two hosts lines above, with your IP instead of
   `127.0.0.1`.

Before opening it to the internet:

- Once your friends have accounts, set `"autoaccount": false` in `config.json`
  and restart. Otherwise anyone can make one.
- Passwords are stored weakly. Tell players not to reuse real ones.
- Never open ports 26080 (admin panel) or 3306 (database) to the internet.
- Change the database password from `gurimoon` in both MariaDB and `config.json`.
- Keep `data\esp-session.key` private. It opens the admin panel.
- Play at your own risk
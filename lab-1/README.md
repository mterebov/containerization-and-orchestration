# Лабораторная работа 1 "У нас есть докер дома"
## Подготовка виртуальной машины и сервиса
### Виртуалка
Лаба выполнялась на маке поэтому была поднята ubuntu 24.04 через мультипасс со следующими параметрами:
``` bash
multipass launch 24.04 --name lab1 --cpus 2 --memory 4G --disk 20G
```

### Сервис
Сервис был навайбкожен клодиком на Go:
- Только стандартная библиотека
- Бинарник статический
- `/eat?mb=N` не просто выделяет память, а записывает байт в каждую страницу. `make` даёт только виртуальные адреса, физическую страницу ядро подкладывает при первой записи (page fault), и только такие страницы попадают в RSS и в `memory.current` cgroup. Без записи лимит памяти и OOM в части 3 было бы не на чем показать. Выделенные куски держатся в глобальной переменной, поэтому GC их не освобождает.

```P.S. Максим: я ради интереса внутрь залез - я бы такого не смог написать... Мне религия не позволяет для счетчика заблокировать целый поток и сделать ручку, реализующую утечку памяти```

Клодик также добавил помимо ручек `/eat?mb=N`, `/burn` и `/health` ещё и ручку `/whoami` (печатает pid, ppid, uid, gid, hostname), сказал что она нам будет нужна в части 2 чтобы показать "изнутри PID 1 и root, снаружи обычный процесс" - мы ему верим он умный

## Часть 1 - Запуск напрямую
Бинарник был успешно скомпилирован и загружен (со второго раза) на ВМ
``` bash
# Компиляция
cd lab-1/api
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/api .
```

- `CGO_ENABLED=0` позволяет сделать бинарник статическим и не тащить за собой libc
- `GOOS=linux` и `GOARCH=arm64` собирают бинарник под нужную систему и архитектуру CPU

``` bash
# Загрузка на ВМ
multipass transfer bin/api lab1:api
```

![тут была картинка где там команды всякие про компиляцию и перенос](report/image.png)

Бинарнику были изменены права и он был запущен из соседнего терминала проверили `/health` и `/whoami`
```bash
chmod +x ~/api && ~/api
```

![тут была картинка про права, запуск и проверк энпоинтов](report/image-1.png)

`/health` так же был проверен из браузера

![Тут проверили здоровье сервиса из браузера (к сожалению он без ДМС)](report/image-2.png)

с помощью `pgrep -a api` был проверен PID - он совпал с тем, что отдала ручка `/whoami` как и должно быть

![Тут было видно чта там PID совпадал](report/p1-pid-match.png)

Так же был сделан снимок состояния ДО чтобы смотреть как оно меняется в процессе лабораторной

Что процесс видит:

![Что процесс видит](report/image-3.png)

Сколько ему можно взять:

![Сколько ему можно взять](report/image-4.png)

Что ему можно:

![Что ему можно](report/image-5.png)

Для сравнения: namespaces init (PID 1) и фактические лимиты cgroup, в котором живёт процесс

![namespaces init и лимиты cgroup](report/p1-baseline-ref.png)

## Часть 2 - namespaces
Сначала с помощью

``` bash
pkill -x api
```

был остановлен api иначе curl бы попадал в старый экземпляр

После была произведена попытка ~~поднять молот Тора~~ с помощью команды `unshare -Ur id -u` создать namespace и прописаться там как рутовый пользователь, namespace создался, а дальше, как я понял, прописаться туда рутом нам не позволила какая-то невероятная надстройка AppArmor'а, появившаяся в ubuntu 23.10, которая блокирует подобные действия

![unshare -Ur: запись uid_map отбита AppArmor](report/p2-userns-eperm.png)

~~Один был убит~~ Проклятие сняли отключив эту надстройку AppArmor'а командой `sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0 && unshare -Ur id -u`

![после sysctl та же команда даёт uid 0](report/p2-userns-ok.png)

Сначала нарочно запустили без --mount-proc. pid namespace уже новый, echo $$ внутри даёт 8, но ps читает /proc, а там смонтирован хостовый procfs, привязанный к pid namespace хоста: видны все процессы системы с хостовыми номерами, readlink /proc/self даёт 2719. Вывод: pid namespace изолирует нумерацию процессов, а что покажет ps, определяет mount namespace, то есть какой /proc смонтирован. Заодно виден user namespace: процессы настоящего root показаны как nobody, потому что uid 0 хоста внутрь не отображён, а unshare, api и bash показаны как root, потому что uid 1000 отображён в 0.

![без --mount-proc: ps показывает хостовые процессы, echo $$ даёт 8](report/p2-no-mount-proc.png)

### Запуск в namespaces

```bash
unshare --user --map-root-user --pid --fork --mount --mount-proc --net --uts --ipc ~/api
```

- `--user --map-root-user` стоит первым и без sudo: ядро создаёт user namespace и даёт процессу в нём полный набор capabilities, их хватает, чтобы создать остальные пять. uid 1000 снаружи отображается в 0 внутри.
- `--pid --fork`: `unshare(CLONE_NEWPID)` не переносит вызывающий процесс, новый pid namespace получают только его дети. Поэтому unshare форкается, и PID 1 становится ребёнок, который делает exec в api.
- `--mount --mount-proc`: свежий procfs для нового pid namespace монтируется поверх `/proc` в собственной таблице монтирования, хостовый `/proc` не трогается.
- `--net`, `--uts`, `--ipc`: свой сетевой стек, свой hostname, свои объекты IPC.

Вход внутрь, тоже без sudo, потому что владелец user namespace (uid 1000) имеет в нём все capabilities:

```bash
nsenter --preserve-credentials -t $(pgrep -x api) -U -p -m -n -u -i -- bash
```

Без `--preserve-credentials` nsenter падал на `setgroups failed`: `--map-root-user` записал `deny` в `/proc/PID/setgroups` (ядро требует этого до записи `gid_map` непривилегированным процессом), и `setgroups()`, который nsenter вызывает по умолчанию, в этом namespace невозможен.

![внутри PID 1 и root, снаружи PID 2734 и ubuntu](report/p2-inside-outside.png)

Внутри: `ps aux` показывает три процесса, api это PID 1. `id` даёт uid 0. `ip a` показывает только lo, и тот лежит, пока не поднять руками. После `hostname lab1-inside` и `ip link set lo up` ручка `/whoami` отвечает `pid=1 uid=0 hostname=lab1-inside`.

Снаружи тот же процесс: PID 2734, user ubuntu, uid 1000. `/proc/2734/uid_map` содержит `0 1000 1`, то есть ровно один uid отображён. Hostname хоста по-прежнему lab1. `curl localhost:8080` с хоста не проходит: порт 8080 открыт в другом сетевом стеке.

`ppid=0` у api внутри: родитель, процесс unshare (PID 2733 на хосте), остался в хостовом pid namespace и в новом не имеет номера. Для процессов, которых в namespace нет, ядро возвращает PID 0. Поэтому изнутри не виден даже собственный родитель.

### Что изолировал каждый namespace

| namespace | что изолировал | чем проверили |
|---|---|---|
| pid | нумерацию процессов: api внутри PID 1, снаружи 2734; видны только свои процессы | `ps aux` внутри, `ps -p` снаружи |
| user | uid/gid: 0 внутри это 1000 снаружи; capabilities «root» действуют только внутри | `id`, `/proc/PID/uid_map`, процессы хостового root видны как nobody |
| mnt | таблицу монтирования: свой `/proc`, хостовый не тронут | без `--mount-proc` ps показывал хостовые процессы |
| net | сетевой стек: только lo, порт 8080 с хоста недоступен | `ip a` внутри, curl с хоста |
| uts | hostname: lab1-inside внутри, lab1 снаружи | `hostname` внутри и снаружи |
| ipc | объекты System V IPC и POSIX-очереди | отдельно не проверяли |

Что осталось общим с хостом: корневая файловая система (внутри тот же `/home/ubuntu/api` и тот же `curl`: mount namespace копирует таблицу хоста, а не даёт новый корень, это закроет образ в части 6), ядро, cgroup (не трогали, это часть 3) и время.

## Часть 3 - cgroups
### Шаг 1 - cgroup и переезд в него

Cgroup в v2 это просто каталог в `/sys/fs/cgroup`: сделали `mkdir`, ядро само насыпало внутрь файлы-ручки. Какие контроллеры достанутся новой группе, решает `cgroup.subtree_control` родителя, у корня это `cpu memory pids`, ровно то, что нам нужно.

```bash
sudo mkdir /sys/fs/cgroup/mydocker && cat /sys/fs/cgroup/mydocker/cgroup.controllers
echo $(pgrep -x api) | sudo tee /sys/fs/cgroup/mydocker/cgroup.procs
echo $(pgrep -P $(pgrep -x nsenter)) | sudo tee /sys/fs/cgroup/mydocker/cgroup.procs
```

Переезд это запись PID в `cgroup.procs`, переезжают сразу все потоки процесса. Перенесли и api, и наш внутренний шелл из nsenter: дети наследуют cgroup при fork, так что всё, что мы из этого шелла запустим (curl, stress-ng), окажется под теми же лимитами. Переносили с хоста по хостовому PID, хотя api сидит в чужих namespaces: cgroups и namespaces друг о друге не знают, это две независимые оси, одна про «что процесс видит», другая про «сколько ему можно».

![cgroup создан, api и шелл переехали](report/p3-cgroup-move.png)

`/proc/PID/cgroup` сменился с `session-4.scope` (снимок «до» из части 1) на `/mydocker`. В `cgroup.procs` два PID, а `pids.current` показывает 7: контроллер pids считает не процессы, а задачи ядра, то есть потоки. Go-рантайм в api держит несколько потоков (GC, sysmon, сетевой поллер) плюс один поток bash. Поэтому `pids.max` надо брать с запасом над этим числом, а не «2 процесса», иначе api упрётся в лимит на первом же новом потоке.

### Шаг 2 - CPU: полкора

```bash
echo "50000 100000" | sudo tee /sys/fs/cgroup/mydocker/cpu.max
```

Два числа это квота и период в микросекундах: 50 мс работы на каждые 100 мс, то есть половина ядра. Внутри дёрнули `/burn`, он крутит одну горутину на 100% ядра, api честно хотел целое ядро, а выдали половину.

![/burn изнутри](report/p3-burn.png)

![cpu.stat до и после 5 секунд](report/p3-cpu-stat.png)

Два снимка `cpu.stat` с разницей в 5 секунд: `nr_periods` 96 → 146 (50 периодов по 100 мс = 5 секунд), `nr_throttled` 68 → 105, `throttled_usec` 4.97 с → 7.69 с, `usage_usec` 4.19 с → 6.51 с. То есть за 5 секунд api получил 2.3 секунды процессора (46%, упёрлось в квоту 50%) и 37 из 50 периодов просидел в throttle: квоту выбирает за первые 50 мс периода, остаток ждёт. Процесс при этом не умирает и никаких ошибок не видит, он просто тормозит. В Kubernetes это `limits.cpu`: kubelet пишет то же самое в `cpu.max`, а throttling снаружи виден только по метрике `container_cpu_cfs_throttled_periods_total` и по выросшей латентности.

### Шаг 3 - pids.max против fork-бомбы

```bash
echo 20 | sudo tee /sys/fs/cgroup/mydocker/pids.max
```

20 с запасом над теми 7 потоками, что уже сидят в группе. Fork-бомбу пускали изнутри, из нашего шелла в nsenter, он в том же cgroup, так что всё, что он породит, считается в тот же `pids.current`. Первая попытка упала на `temp-path '.' must be readable and writeable`: после nsenter мы стоим в `/`, а он принадлежит настоящему root, для нашего «root внутри» это чужой `nobody`, писать нельзя. Ещё раз видно, что корень ФС общий с хостом. Ушли в `/tmp`.

```bash
cd /tmp && stress-ng --fork 8 --timeout 10s
```

![stress-ng --fork 8 изнутри](report/p3-forkbomb-stress.png)

![pids.current и pids.events во время и под конец прогона](report/p3-forkbomb-pids.png)

Сам stress-ng отчитался `passed: 8, failed: 0`: отказ `fork()` с `EAGAIN` он считает штатной ситуацией и молча пробует снова, поэтому его резюме выглядит как успех. Правда в cgroup: `pids.current` во время прогона стоял ровно на потолке 20 (7 потоков api и шелла, stress-ng с восемью воркерами, и всё, на детей места уже почти нет), а `pids.events` показывал `max 37465` и под конец `max 55217`: столько раз за 10 секунд ядро отказало в `fork`. Бомба не размножилась, api при этом жив и отвечает. В Docker это `--pids-limit`, в Kubernetes `podPidsLimit` у kubelet, оба пишут в тот же `pids.max`.

### Шаг 4 - память и OOM

```bash
echo 64M | sudo tee /sys/fs/cgroup/mydocker/memory.max; echo 0 | sudo tee /sys/fs/cgroup/mydocker/memory.swap.max
```

`memory.swap.max=0` нужен, потому что иначе при упоре в лимит ядро сначала вытесняет страницы в своп, а не убивает. В нашей VM свопа нет, но скрипт из части 5 на это полагаться не должен; Kubernetes по умолчанию делает то же самое. Сначала съели 30 МБ:

![/eat?mb=30](report/p3-eat-30.png)

![memory.current после 30 МБ](report/p3-memory-current.png)

`memory.current` = 33124352 байт, это 31.6 МиБ: 30 МиБ от `/eat` плюс немного нового. Память, которую api занял до переезда в cgroup, осталась на счету старой группы: списание идёт на ту группу, где страница была выделена, при переезде процесса оно не переезжает. Потом попросили ещё 50:

![OOM: memory.events и dmesg](report/p3-oom.png)

Curl получил `Killed`, и это убили не curl. В `memory.events`: `max 32` (32 раза группа упёрлась в лимит, и ядро пыталось что-то вытеснить, а вытеснять без свопа нечего), `oom 1`, `oom_kill 1`. В dmesg ядро перечислило задачи группы (api, bash, curl), выбрало самую жирную и написало `Memory cgroup out of memory: Killed process 1414 (api) anon-rss:64512kB`: 63 МиБ anon как раз и есть потолок 64M. Вместе с api умер и наш внутренний шелл: api это PID 1 своего pid namespace, а когда init namespace погибает, ядро добивает всех остальных в нём. Это и есть `OOMKilled` в Kubernetes: `limits.memory` превращается в тот же `memory.max`, kubelet видит код выхода 137 (SIGKILL), перезапускает контейнер, а если это повторяется, получается `CrashLoopBackOff`.

### Итог части 3

Память бьёт убийством, CPU бьёт задержкой, pids просто отказывает в `fork`. Все три лимита это файлы в одном каталоге cgroup, Docker и kubelet пишут в них ровно то же самое, что мы руками.

## Часть 4 - привилегии
### Шаг 1 - что умеет root внутри user namespace

Api у нас живёт в user namespace как «root», и прежде чем что-то резать, стоило понять, что этот root вообще может. Свежий namespace со своим hostname:

```bash
unshare -Ur -u bash
```

![root в user namespace: полный набор, hostname меняется, время нет](report/p4-userns-caps.png)

`CapEff` полный, все 41 бит, `hostname demo` проходит, а `date -s` отбивается с `Operation not permitted`, хотя набор тот же. Разница в том, где ядро проверяет capability. Для `sethostname` нужен `CAP_SYS_ADMIN` в том user namespace, которому принадлежит uts namespace, а он наш. Для `clock_settime` нужен `CAP_SYS_TIME` в корневом user namespace хоста: часы у ядра одни на всех, их не изолировать. Так что user namespace сам по себе уже отрезает всё, что не делится по namespace (время, модули ядра, reboot), а root внутри это root только над своими namespace.

### Шаг 2 - отрезать всё

```bash
setpriv --bounding-set=-all --inh-caps=-all bash
```

![после setpriv: CapEff и CapBnd нули, hostname не меняется](report/p4-caps-dropped.png)

`CapEff` и `CapBnd` нули, `hostname demo2` уже не проходит (hostname пишет «you must be root», хотя мы всё ещё uid 0, он просто так переводит EPERM). Bounding set это потолок, и он только опускается: системного вызова «поднять обратно» нет. Capabilities попадают в процесс двумя путями, через наследование при exec и через ambient-набор, и оба маскируются bounding set. Правило «root при exec получает всё» проходит через ту же маску. Поэтому ноль это навсегда, для процесса и всех его детей: ~~прыгнул в колодец без рук и ног~~.

### Шаг 3 - api без единой capability

Api они не нужны: порт 8080 выше 1024, файлы свои, ничего привилегированного он не делает. Поэтому запускаем его в тех же namespaces, но через setpriv:

```bash
unshare --user --map-root-user --pid --fork --mount --mount-proc --net --uts --ipc setpriv --bounding-set=-all --inh-caps=-all ~/api
```

![api стартует как PID 1 и отвечает на /health](report/p4-api-nocaps-start.png)

![изнутри: lo поднят, /health отвечает](report/p4-api-nocaps-health.png)

`ip link set lo up` изнутри проходит, потому что шелл из nsenter получает свои capabilities при входе в user namespace, ему мы ничего не резали. Api при этом работает как ни в чём не бывало. Снаружи:

![все наборы capabilities у api нулевые, seccomp пока 0](report/p4-api-status.png)

Все пять наборов по нулям, это пара к снимку «до» из части 1, где `CapBnd` был полный. `Seccomp: 0` пока, это следующий шаг.

### Шаг 4 - seccomp

Seccomp-профиль это список системных вызовов и что с ними делать: ядро сверяет с ним каждый syscall процесса и либо пропускает, либо возвращает ошибку, либо убивает. Capabilities отвечали «что процессу можно», seccomp отвечает «какими вызовами ядра он вообще может пользоваться». Профиль [seccomp.json](seccomp.json) в формате Docker: всё разрешено, кроме восьми вызовов, на них EPERM. `mkdir`/`mkdirat` там ради наглядной демонстрации, остальные это то, что api не нужно и что закрывает опасное: смена hostname, монтирование, отладка чужих процессов (`ptrace`), перезагрузка, загрузка модулей ядра. У Docker по умолчанию наоборот, белый список из ~350 вызовов, но для лабы хватило чёрного.

Докера у нас нет, навесить профиль некому, поэтому навешивает маленький загрузчик [seccomp_exec.py](seccomp_exec.py): читает JSON, собирает фильтр через libseccomp, грузит его в ядро и делает `exec` в команду. Фильтр наследуется всеми детьми, и снять его процесс не может, как и вернуть capabilities. Сначала проверили на шелле:

```bash
python3 ~/seccomp_exec.py ~/seccomp.json -- bash
```

![mkdir отбит seccomp, Seccomp: 2](report/p4-seccomp-shell.png)

`mkdir /tmp/zz` отбит с `Operation not permitted`, хотя у этого шелла есть все capabilities, а в `/tmp` писать можно кому угодно: это не права, это фильтр. В статусе `Seccomp: 2` (режим filter, в части 1 здесь был 0), `Seccomp_filters: 1` и `NoNewPrivs: 1`: libseccomp ставит этот флаг, чтобы фильтр можно было грузить без capabilities, а заодно он запрещает получать привилегии через setuid-бинарники.

Потом api, полная цепочка «namespaces, seccomp, capabilities, api»:

```bash
unshare --user --map-root-user --pid --fork --mount --mount-proc --net --uts --ipc python3 ~/seccomp_exec.py ~/seccomp.json -- setpriv --bounding-set=-all --inh-caps=-all ~/api
```

![api стартует под профилем](report/p4-seccomp-api-start.png)

![api: capabilities нули, Seccomp: 2](report/p4-seccomp-api-status.png)

![api под профилем отвечает](report/p4-seccomp-api-health.png)

Api работает: ему хватает `read`, `write`, `accept` и прочего обычного, ничего из чёрного списка он не зовёт.

### Итог части 4

Что закрывает каждый механизм:

- **user namespace**: root внутри это root только над своими namespace. Всё, что не делится по namespace (время, модули ядра, reboot), отрезано ещё до capabilities.
- **capabilities**: привилегированные действия внутри namespace (сменить hostname, поднять интерфейс, монтировать). Bounding set опущен в ноль, обратно не поднять.
- **seccomp**: сами системные вызовы. Даже если бы capabilities вернулись, `mount` или `ptrace` упёрлись бы в фильтр. Это второй, независимый забор.

Docker делает ровно то же: `--cap-drop`/`--cap-add` правят bounding set, `--security-opt seccomp=profile.json` грузит такой же JSON через libseccomp, по умолчанию с белым списком.

## Часть 5 - собираем свой Docker
### mydocker.sh

Все команды частей 2–4 собраны в [mydocker.sh](mydocker.sh): cgroup с лимитами, переезд в него и цепочка `unshare → lo up → seccomp → setpriv → api`. Два момента, ради которых скрипт устроен именно так. В части 3 мы переносили api в cgroup уже после старта, и между запуском и переносом он жил без лимитов. Поэтому скрипт сажает в cgroup сам себя (`echo $$ > cgroup.procs`) и только потом делает `exec`: дети наследуют группу, окна нет, так делают и настоящие runtime'ы. А `ip link set lo up` стоит внутри цепочки до `setpriv`: после сброса capabilities поднять интерфейс уже некому. `exec` на каждом шаге не меняет PID, поэтому api в итоге PID 1 своего namespace, а не ребёнок какого-нибудь sh.

Запуск `./mydocker.sh` в одном окне, проверка во втором:

![mydocker.sh: /health отвечает, cgroup /mydocker, capabilities нули, seccomp 2](report/p5-mydocker-check.png)

### Тот же api через docker run

Dockerfile будет в части 6, а пока статический бинарник просто подсунут в alpine через bind mount. Лимиты и права те же, что в скрипте: `--memory-swap` равный `--memory` это наш `memory.swap.max=0`, `--cpus 0.5` это `50000 100000`, профиль seccomp тот же файл.

```bash
docker run -d --name api -p 8080:8080 -v /home/ubuntu/api:/api:ro --memory 64m --memory-swap 64m --cpus 0.5 --pids-limit 20 --cap-drop ALL --security-opt seccomp=/home/ubuntu/seccomp.json alpine /api
```

![docker run и curl с хоста](report/p5-docker-run.png)

Первое отличие видно сразу: `curl localhost:8080/health` с хоста работает без всяких nsenter. (Docker Hub из VM доставался через раз, часть его адресов на маке уходит в VPN, поэтому в `/etc/docker/daemon.json` прописаны `registry-mirrors`.)

### Сравнение

Оба api работали одновременно, с хоста снимали одно и то же для обоих процессов:

```
                 mydocker.sh                        docker run
USER / UID       ubuntu / 1000                      root / 0
user ns          свой (4026532228)                  хостовый (4026531837)
pid mnt net      свои                               свои
uts ipc
cgroup ns        хостовый                           свой
cgroup           /mydocker                          /system.slice/docker-<id>.scope
memory.max       67108864 (64M), swap 0             67108864 (64M), swap 0
cpu.max          50000 100000                       50000 100000
pids.max         20                                 20
CapEff / CapBnd  0 / 0                              0 / 0 (без --cap-drop было бы a80425fb, 14 штук)
Seccomp          2, один фильтр, NoNewPrivs 1       2, один фильтр, NoNewPrivs 0
AppArmor         unconfined                         docker-default (enforce)
корень ФС        хостовый /, те же mounts           overlay из образа alpine, свои /proc и /dev
сеть             только lo                          eth0 172.17.0.2 ↔ veth ↔ docker0, DNAT :8080
```

**Что совпадает.** Главное: тот же набор namespaces (кроме user и cgroup), те же четыре файла лимитов с теми же значениями, те же нули в capabilities и тот же seccomp-профиль с теми же восемью правилами. Ядро под обоими делает одно и то же, Docker тут не добавляет никакой магии, он пишет в те же файлы, что мы руками.

**Чего не хватает нашему скрипту.**

- Своей файловой системы: api видит весь хостовый корень. У Docker корень это overlay из образа, в него делается `pivot_root`, хост снаружи не виден. Это закроет часть 6.
- Сети наружу: у нас пустой netns с одним lo, до сервиса не достучаться. Docker делает veth-пару, вешает её на bridge `docker0`, выдаёт 172.17.0.2 и пишет DNAT-правило в iptables для `-p 8080`.
- cgroup namespace: внутри нашего контейнера `/proc/self/cgroup` показывает хостовый путь и видно всё дерево, Docker его прячет.
- AppArmor-профиля: докерный процесс под `docker-default`, наш `unconfined`. Это третий слой из лекции, LSM, у нас его нет.
- Жизненного цикла: имени, `stop`/`rm`/`exec`/`logs`, уборки cgroup после смерти (наш `/mydocker` остаётся висеть), restart policy, `--init` для зомби.

**Что у нас есть, а у Docker по умолчанию нет.** Мы rootless: снаружи api это uid 1000, а у Docker без `userns-remap` root в контейнере это настоящий root хоста, и держит его только набор capabilities плюс seccomp. И `NoNewPrivs: 1` у нас выставлен автоматически libseccomp, у Docker его надо просить отдельно через `--security-opt no-new-privileges`.

**Что Docker делает сверх этого.** Образы: pull по digest, слои, кеш, overlay. Демон и стек `dockerd → containerd → containerd-shim → runc`: runc делает ровно то, что наш скрипт, и выходит, а shim остаётся держать stdio и код выхода, поэтому у докерного api родитель это shim, а не клиент. Логи (`json-file`), сеть с DNS между контейнерами, volumes, healthcheck, env, и API, который снаружи дёргает kubelet через CRI.

## Часть 6 - образы
### Dockerfile и сборка

Dockerfile'ы лежат рядом с кодом в [api/](api/). [Dockerfile.fat](api/Dockerfile.fat) собирает и запускает в одном образе `golang:1.22`. [Dockerfile](api/Dockerfile) это multi-stage: та же сборка в стадии `build`, а в итоговый образ `FROM scratch` копируется один бинарник. `CGO_ENABLED=0` в обоих обязателен: в `scratch` нет libc, пакет `net` при включённом cgo линкуется динамически, и такой бинарник там не стартует. `go.mod` копируется отдельной строкой раньше кода, чтобы слой с зависимостями переживал правки `main.go`. В `.dockerignore` лежит `bin/`, иначе локальный бинарник на 8 МБ уезжал бы в контекст каждой сборки.

```bash
multipass transfer -r lab-1/api lab1:api-src
cd ~/api-src && docker build -t api:fat -f Dockerfile.fat . && docker build -t api:slim .
```

Собирал legacy builder: в пакете `docker.io` нет плагина buildx. Это тот самый линейный кеш из лекции, BuildKit ставится отдельно пакетом `docker-buildx`.

### Размер и слои

![docker images api и docker history](report/p6-images.png)

| образ | disk usage | content size | строк в history | слоёв с содержимым |
|---|---|---|---|---|
| api:fat | 1.3 ГБ | 308 МБ | 16 | 11 |
| api:slim | 10.7 МБ | 3.84 МБ | 3 | 1 |

Разница в сто с лишним раз. В `api:fat` весь toolchain Go из `golang:1.22` (сам образ 1.22 ГБ) плюс кеш `go build`. В `api:slim` один слой на 6.9 МБ, это бинарник, а `EXPOSE` и `ENTRYPOINT` это строки по 0 байт, в них только метаданные конфига. Куда делись слои стадии `build`: они остались в кеше демона как промежуточные образы, а итоговый образ собирается только из последней стадии. `FROM scratch` даёт ноль слоёв, `COPY --from=build` добавляет один, и всё. Стадия `build` для итогового образа только источник файла.

### Кеш

Пересборка без изменений: все девять шагов `Using cache`, ничего не выполняется.

![пересборка без изменений: всё из кеша](report/p6-cache-nochange.png)

Потом в `main.go` добавлен комментарий и сборка повторена:

```
Sending build context to Docker daemon  12.29kB
Step 1/9 : FROM golang:1.22 AS build
Step 2/9 : WORKDIR /src                           ---> Using cache
Step 3/9 : COPY go.mod ./                         ---> Using cache
Step 4/9 : COPY main.go ./                        ---> новый слой
Step 5/9 : RUN CGO_ENABLED=0 go build -o /api .   ---> Running in 196bb1a8a437
Step 6/9 : FROM scratch
Step 7/9 : COPY --from=build /api /api            ---> новый слой
Step 8/9 : EXPOSE 8080                            ---> Running in e21cd7eaf4e3
Step 9/9 : ENTRYPOINT ["/api"]                    ---> Running in 0d4e99099874
Successfully built bd7f1fc447fb
```

Кеш отвалился ровно на `COPY main.go`: для `COPY` ключ кеша это контрольная сумма файлов, она изменилась, и всё ниже по Dockerfile пересобирается, потому что каждый следующий слой зависит от предыдущего. `WORKDIR` и `COPY go.mod` выше по файлу остались из кеша, ради этого `go.mod` и копируется отдельно. Первая попытка этого опыта показала всё из кеша: второй `multipass transfer -r` положил папку вложенно в `api-src/api/`, и собиралась старая копия, заодно с бинарником из `bin/` в контексте на 8 МБ. Разложили как надо, контекст снова 12 КБ.

### Запуск slim-образа

![api:slim вместо контейнера из части 5](report/p6-run-slim.png)

### Файл в контейнере и volume

![файл без volume: записали, пересоздали, нет](report/p6-no-volume.png)

Записали `/tmp/f`, удалили контейнер, запустили новый из того же образа: файла нет. Он жил в writable-слое контейнера, верхнем слое overlayfs, и умер вместе с ним. `docker inspect -f '{{.GraphDriver...}}'` на скриншоте не сработал: Docker 29 держит образы в containerd (containerd image store), и этого поля в inspect больше нет, вместо него `ls` показал хостовый `/tmp`. Слой всё равно находится через mount контейнера:

```
$ sudo grep ' / overlay ' /proc/$(docker inspect -f '{{.State.Pid}}' t2)/mounts
... upperdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/56/fs ...
$ sudo ls -l /var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/56/fs/tmp
-rw-r--r-- 1 root root 6 Oct 24 15:53 f
$ docker rm -f t2 && sudo ls /var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/56/fs
ls: cannot access '...': No such file or directory
```

Файл лежит на хосте в `upperdir` этого контейнера, `docker rm` сносит каталог целиком.

![файл в volume переживает контейнер](report/p6-volume.png)

С volume файл пережил два контейнера: `apidata` это обычный каталог `/var/lib/docker/volumes/apidata/_data` на хосте, который bind-монтируется в `/data` поверх overlay. Он контейнеру не принадлежит, поэтому и не удаляется с ним. Так же устроен bind mount, и так же в Kubernetes PersistentVolume для postgres из примера курса: данные не в слое контейнера, а снаружи.

## Часть 7 - когда контейнера мало: gVisor
### Установка

Из VM до бакета gVisor не достучаться (адреса Google на маке уходят в VPN), поэтому `runsc.deb` для arm64, release 20261019.0, скачан на Mac, сверен по SHA256 с индексом apt-репозитория и переброшен в VM через `multipass transfer`. Дальше:

```bash
sudo apt install -y ~/runsc.deb && sudo runsc install && sudo systemctl restart docker
```

`runsc install` дописал runtime в `/etc/docker/daemon.json`, в `docker info` появился `runsc` рядом с `runc`.

### Тот же образ под gVisor

```bash
docker run -d --name api-gvisor --runtime=runsc -p 8081:8080 api:slim && curl localhost:8081/health
```

![api:slim под runsc отвечает](report/p7-gvisor-run.png)

Образ тот же, Dockerfile тот же, в сервисе не изменилось ничего: runtime подменяется одним флагом. Изнутри (через alpine, в scratch нет шелла):

![uname -r и dmesg под runc и под runsc](report/p7-gvisor-kernel.png)

Под обычным runtime `uname -r` отдаёт хостовые `6.8.0-147-generic`. Под gVisor `4.19.0-gvisor`, а `dmesg` показывает «загрузку» ядра, которого не существует: это Sentry, ядро gVisor на Go, оно прикидывается Linux 4.19. Процесс внутри разговаривает с ним, а не с хостом.

### Взгляд с хоста

![с хоста: у api процесс api, у api-gvisor процесс gvisor_sentry](report/p7-host-view.png)

```
                   docker run (runc)                docker run --runtime=runsc
процесс на хосте   api, PID 5820, 7 потоков         gvisor_sentry, PID 5510, 17 потоков и дочерние
pgrep -x api       находит                          не находит: процесса api на хосте нет
user namespace     хостовый                         свой, runsc создал его сам
CapEff             a80425fb, 14 дефолтных           8001f: chown, dac_*, fowner, fsetid, sys_ptrace
Seccomp            2, профиль Docker на api         2, собственный фильтр Sentry
ядро внутри        6.8.0-147-generic, хостовое      4.19.0-gvisor, Sentry
```

### Как gVisor устроен иначе

У обычного контейнера api это обычный процесс хоста под namespaces, и каждый его системный вызов исполняет ядро хоста. У gVisor между процессом и ядром стоит Sentry: отдельный процесс (`gvisor_sentry`), который реализует системные вызовы Linux сам, в userspace, на Go. Приложение живёт в дочернем процессе Sentry, его syscalls перехватываются (платформа systrap, через seccomp-ловушки) и уходят в Sentry, а не в ядро. Сам Sentry к хостовому ядру обращается узким набором из нескольких десятков вызовов под собственным seccomp-фильтром, к файлам ходит через отдельный процесс gofer, сеть реализует сам (netstack). Поэтому на хосте нет процесса `api`, а есть `gvisor_sentry` с потоками и детьми, и поэтому `uname` внутри «врёт» про 4.19.

Почему это считается более изолированным: поверхность атаки на ядро хоста сужается с «все вызовы, которые разрешит seccomp» до нескольких десятков вызовов Sentry, а сам Sentry это непривилегированный процесс на memory-safe языке в своём user namespace с шестью capabilities. Баг в реализации какого-нибудь syscall ломает Sentry, а не ядро. Цена: каждый syscall дороже (перехват и переключение контекста), и часть вызовов не реализована, так что не всё под gVisor вообще запускается.

### Что обычный контейнер всегда делит с хостом

Ядро. Контейнер это процесс с наворотами: namespaces это метки на объектах внутри того же ядра, cgroups это его же учёт, а каждый системный вызов из контейнера исполняется кодом ядра хоста, тем самым, что показал `uname -r`. Поэтому уязвимость ядра, до которой можно дотянуться системным вызовом, это побег на хост со всем, что на нём есть, и seccomp только сужает поверхность, а не убирает её. Это и есть предел изоляции контейнера: она ровно такая, как надёжность ядра. gVisor подкладывает своё ядро между процессом и хостовым, виртуалки (Kata, Firecracker) дают отдельное настоящее ядро, и то и другое это уже не «просто процесс».

### Три варианта рядом

| | mydocker.sh | docker run (runc) | docker --runtime=runsc |
|---|---|---|---|
| кто исполняет syscalls | ядро хоста | ядро хоста | Sentry, затем узкий набор в ядро |
| процесс на хосте | api, uid 1000 | api, root | gvisor_sentry, root в своём userns |
| user namespace | свой | нет | свой |
| rootfs | хостовый | overlay из образа | overlay из образа через gofer |
| сеть | только lo | veth, bridge, NAT | netstack в Sentry плюс veth |
| цена | ничего | ничего заметного | перехват каждого syscall |

## Часть 8 - мониторинг
### Откуда метрики

Метрики контейнеров собирает cAdvisor: он читает те же файлы cgroup, что мы в части 3 смотрели руками (`memory.current`, `cpu.stat`, `pids.current`), и отдаёт их Prometheus'у. Образ cAdvisor живёт на `gcr.io`, куда из VM не достучаться, поэтому он стоит бинарником v0.60.6 с GitHub (скачан через Mac) и оформлен как systemd-сервис на порту 8088:

```
ExecStart=/home/ubuntu/cadvisor -port 8088 -housekeeping_interval=5s -enable_metrics=cpu,memory,oom_event,process,network -store_container_labels=false
```

Три грабли по дороге. Имена контейнеров: Docker 29 хранит образы в containerd, и с флагом `-containerd-namespace=moby` cAdvisor называл контейнеры по ID, а без него подхватилась Docker-фабрика, и в метриках появились `name="api"` и `name="api-gvisor"`. Метрики процессов (`container_processes`, `container_threads`, `container_threads_max`) входят в набор `process`, выключенный по умолчанию, включены через `-enable_metrics`. И `container_cpu_cfs_*` cAdvisor отдаёт только контейнерам с CPU-квотой: `api` из части 6 запускался без `--cpus`, поэтому перезапущен с теми же лимитами, что в части 5, плюс `--restart=on-failure`, чтобы после OOM подниматься самому, как это делает kubelet.

Prometheus и Grafana подняты через [monitoring/docker-compose.yml](monitoring/docker-compose.yml) с `network_mode: host`: Prometheus скрейпит `localhost:8088` раз в 5 секунд ([prometheus.yml](monitoring/prometheus.yml)), источник данных и дашборд подложены в Grafana через provisioning ([grafana-datasource.yml](monitoring/grafana-datasource.yml), [dashboards/api.json](monitoring/dashboards/api.json)).

### Дашборд

Шесть панелей, всё про один контейнер `api`:

| панель | запрос | зачем |
|---|---|---|
| память: working set и лимит | `container_memory_working_set_bytes`, `container_spec_memory_limit_bytes` | расстояние до OOM; working set, а не RSS, потому что именно его ядро считает невытесняемым |
| CPU в ядрах и квота | `rate(container_cpu_usage_seconds_total[1m])`, `container_spec_cpu_quota / container_spec_cpu_period` | сколько реально получает против того, сколько разрешено |
| доля throttled-периодов | `rate(container_cpu_cfs_throttled_periods_total[1m]) / rate(container_cpu_cfs_periods_total[1m])` | то же, что в части 3 читали из `cpu.stat`: доля периодов, в которых квота кончилась |
| потоки и pids.max | `container_threads`, `container_threads_max` | запас до отказа в `fork`, в части 3 это были 7 потоков при лимите 20 |
| OOM kills | `container_oom_events_total` | факт убийства, в Kubernetes это `OOMKilled` |
| api и api-gvisor рядом | `container_memory_working_set_bytes{name=~"api\|api-gvisor"}` | gVisor-контейнер виден теми же метриками: Sentry живёт в такой же cgroup |

![дашборд под нагрузкой](report/p8-dashboard.png)

Под нагрузкой (`/burn` и `/eat?mb=30`): throttled-доля 93%, использование CPU упёрлось в 0.5 ядра, память 33 МБ из 64, потоков 8 из 20.

### Три метрики для алертов

Правила в [monitoring/alerts.yml](monitoring/alerts.yml), Prometheus загружает их и считает каждые 15 секунд.

1. **Память выше 90% лимита в течение минуты** (`container_memory_working_set_bytes / (container_spec_memory_limit_bytes > 0) > 0.9`). Ловит приближение к потолку, пока ещё есть время что-то сделать: поднять лимит, найти утечку, отмасштабировать. Важно потому, что память, в отличие от CPU, не замедляет, а убивает: следующий шаг ядра это OOM kill без предупреждения.
2. **Доля throttled-периодов выше 25% в течение пяти минут.** Ловит слишком низкий CPU-лимит. Важно потому, что throttling невидим: процесс жив, ошибок нет, health-check зелёный, а латентность растёт, и пользователи замечают это раньше, чем мониторинг по ошибкам.
3. **OOM kill за последние десять минут** (`increase(container_oom_events_total[10m]) > 0`). Ловит уже случившееся. Первый алерт может не успеть, всплеск памяти укладывается в секунды, а здесь факт: сервис падал, всё, что было в памяти, потеряно, в Kubernetes это `OOMKilled` и `CrashLoopBackOff`, если повторяется.

Чего нет и почему: нет алерта на число потоков, при `pids.max=20` и 8 потоках Go-рантайма он ловил бы только fork-бомбу, которой в сервисе нет. Нет и алерта на абсолютное использование CPU: высокий CPU сам по себе не проблема, проблема это throttling.

### Проверка алертов

Под `/burn` правило про throttling провисело в `pending` ровно пять минут своего `for` и перешло в `firing` при доле throttled-периодов 95%. Потом `/eat?mb=50`: ядро убило api, `--restart=on-failure` поднял его заново (у контейнера `RestartCount` 1), в новой cgroup `memory.events` чистые, а cAdvisor счётчик `container_oom_events_total` для контейнера сохранил, он стал 1, и `ContainerOOMKilled` загорелся. Одна грабля вылезла сразу: первая версия правила про память горела на Prometheus, Grafana и api-gvisor, у которых лимита нет. У таких контейнеров cAdvisor отдаёт `container_spec_memory_limit_bytes` = 0, деление даёт `+Inf`, и `+Inf > 0.9` истина. Поэтому в правиле знаменатель обёрнут в `(... > 0)`: контейнеры без лимита из выражения выпадают.

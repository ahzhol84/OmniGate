# aiqiangua_x8/worker.go 中文伪代码翻译

---

公共结构体 名字是ConfigItem（字段如下）{
  字符串 ComponyName（配置项：公司名称）;
  字符串 ListenAddr（配置项：监听地址）;
  字符串 ReceivePath（配置项：接收路径）;
  字符串 PathPrefix（配置项：路径前缀）;
  字符串 DeviceType（配置项：设备类型）;
  字符串 UniquePrefix（配置项：唯一标识前缀）;
  字符串数组 AllowedIPs（配置项：允许访问IP列表）;
}

公共结构体 名字是Worker（字段如下）{
  ConfigItem数组 configs（运行配置列表）;
  HTTP服务器数组 servers（启动后的服务器列表）;
}

---

公共方法 没有返回参数 名字是Init（入参：configs为原始JSON数组）{
  循环每一个原始配置并做如下操作;
    反序列化为ConfigItem，如果失败则返回“配置解析失败”错误;
    如果ListenAddr为空，则设置为":8090";
    如果ReceivePath为空，则设置为"/push/aiqiangua/x8";
    调用路径规范化工具处理ReceivePath;
    如果PathPrefix为空，则设置为"/push/aiqiangua/x8";
    调用路径规范化工具处理PathPrefix;
    如果DeviceType为空，则设置为"AIQIANGUA_X8";
    把配置加入Worker.configs;
    调用日志打印工具（打印监听地址、接收路径、路径前缀）;
  全部成功后返回空错误;
}

公共方法 没有返回参数 名字是Start（入参：ctx上下文，out为设备数据输出通道）{
  创建等待组;
  循环Worker.configs，对每个配置启动一个协程执行runServer;
  创建done信号通道;
  启动协程等待全部server协程结束后关闭done;
  等待以下任意事件:
    如果ctx结束:
      循环全部servers，逐个执行5秒超时优雅关闭;
      等待done结束;
    如果done先结束:
      直接返回;
}

公共方法 没有返回参数 名字是runServer（入参：ctx、out、index、cfg）{
  创建HTTP路由复用器mux;
  定义handler，内部调用handlePush;
  mux绑定cfg.ReceivePath到handler;
  mux绑定cfg.PathPrefix + "/"到handler;
  创建HTTP服务器（地址为cfg.ListenAddr，处理器为mux）;
  把服务器加入Worker.servers;
  调用日志打印工具（打印监听信息）;
  启动协程执行ListenAndServe;
    如果报错且不是正常关闭错误，则打印退出日志;
  当前方法阻塞等待ctx结束后退出;
}

公共方法 没有返回参数 名字是handlePush（入参：rw、req、out、cfg、workerIndex）{
  如果请求方法不是POST且不是GET:
    返回405“method not allowed”;
  获取客户端IP;
  如果配置了AllowedIPs且当前IP不在白名单中:
    返回403“forbidden”;
  读取请求体;
    如果读取失败则返回400“read body failed”;
  关闭原始Body并把读取内容重新放回req.Body（供后续解析使用）;
  调用表单解析工具parseForm;
    如果解析失败则返回400“invalid form body”;
  调用步数字段归一化工具normalizeStepNumberField;
  计算事件类型eventType（来自query/form/path推断）;
  计算上报设备类型reportedDeviceType（优先form，否则用配置默认）;
  计算设备IMEI;
  计算deviceSourceID（优先IMEI，否则从其它设备ID字段查找）;
  如果deviceSourceID仍为空，则设为"unknown";
  如果deviceIMEI为空，则用deviceSourceID补齐;
  计算resolvedUniquePrefix（优先cfg.UniquePrefix，空则用cfg.ComponyName）;
  构造physicalKey = 设备类型 + imei;
  构造logicalKey = physicalKey + 事件类型;
  调用设备唯一ID工具生成deviceID（基于physicalKey）;
  调用设备唯一ID工具生成uniqueID（基于logicalKey）;
  解析事件时间eventTime;
  调用载荷构建工具buildPayload（根据事件类型筛字段）;
    如果返回keep=false:
      返回200 JSON：{"code":0,"msg":"ignored"};
      打印忽略日志;
      结束;
  把payload对象序列化为JSON;
    如果失败返回500“payload marshal failed”;
  构建设备数据对象DeviceData（包含deviceID/uniqueID/deviceType/dataType/timestamp/payload/company）;
  向out通道写入，二选一处理:
    写入成功:
      返回200 JSON：{"code":0,"msg":"ok"};
      打印接收日志;
    3秒超时:
      返回503“busy”;
      打印通道繁忙日志;
}

---

公共方法 返回（映射,布尔） 名字是buildPayload（入参：eventType、values、deviceIMEI、eventTime）{
  定义事件到允许字段列表的映射表allowed:
    EXERCISE_HEART_RATE -> average_heartrate, imei, max_heartrate, min_heartrate, step, time_begin;
    BLOOD_OXYGEN -> bloodoxygen, imei, time_begin;
    BLOOD_PRESSURE -> dbp, imei, sbp, time_begin;
    SLEEP -> awake_time, deep_sleep, imei, light_sleep, time_begin, time_end, total_sleep;
    STEP -> imei, step_number_value, time_begin;
    HEART_RATE -> heartrate, imei, time_begin;
    LOCATION -> address, city, imei, lat, lon, time_begin;
  如果eventType不在映射里:
    返回（空, false）;
  创建payload映射;
  循环事件允许字段，逐个填充值:
    字段是imei:
      调用标识归一化工具处理deviceIMEI，非空则写入;
    字段是time_begin:
      优先取values.time_begin;
      否则若eventTime有效，则格式化为"yyyy-MM-dd HH:mm:ss"写入;
    字段是heartrate:
      尝试转整数，成功则写整数，失败写原字符串;
    其它字段:
      若values对应值非空则写入;
  如果payload为空:
    返回（空, false）;
  返回（payload, true）;
}

公共方法 返回（映射,错误） 名字是parseForm（入参：req）{
  读取Content-Type并转小写;
  如果是multipart/form-data:
    按8MB上限解析多段表单;
  否则:
    解析普通表单;
  若解析失败返回错误;
  创建result映射;
  循环req.Form每个键:
    取最后一个值放入result;
  返回result;
}

公共方法 返回字符串 名字是resolveDeviceSourceID（入参：values）{
  按顺序检查键 imei、deviceid、device_id、devID、devId;
  返回第一个非空值;
  如果都为空返回空字符串;
}

公共方法 没有返回参数 名字是normalizeStepNumberField（入参：values）{
  如果values为空直接返回;
  如果step_number_value非空:
    写回step_number_value;
    如果stepNumber为空则补成相同值;
    删除value;
    返回;
  如果stepNumber非空:
    stepNumber与step_number_value都设为该值;
    删除value;
    返回;
  如果value非空:
    step_number_value与stepNumber都设为该值;
    删除value;
}

公共方法 返回字符串 名字是resolveDeviceIMEI（入参：values）{
  按顺序检查 imei、deviceid、device_id、devID、devId;
  每个值先做标识归一化;
  返回第一个非空值;
  若没有返回空字符串;
}

公共方法 返回字符串 名字是resolveReportedDeviceType（入参：values、fallback）{
  按顺序检查 device_type、devicetype、model、type_name;
  返回第一个非空值并做数据类型归一化;
  若都为空则返回fallback归一化后的值;
}

公共方法 返回字符串 名字是normalizeIdentityPart（入参：value）{
  去除首尾空白;
  若为空返回空字符串;
  把“|”替换为“_”;
  返回处理后的字符串;
}

公共方法 返回字符串 名字是buildPhysicalKey（入参：deviceType、imei）{
  设备类型做归一化;
  imei做标识归一化;
  如果imei为空则设为"unknown";
  返回“设备类型|imei”;
}

公共方法 返回字符串 名字是buildLogicalKey（入参：physicalKey、eventType）{
  physicalKey做标识归一化;
  eventType做数据类型归一化;
  返回“physicalKey|eventType”;
}

公共方法 返回字符串 名字是detectEventType（入参：u、path、form）{
  优先从query参数event读取;
  其次从query参数type_name读取;
  再从form参数event/type_name/event_type/data_type读取;
  若以上都没有，按路径后缀映射:
    /location -> LOCATION;
    /sos -> SOS;
    /heartrate -> HEART_RATE;
    /step -> STEP;
    /sleep -> SLEEP;
    /power -> POWER;
    /bloodpressure -> BLOOD_PRESSURE;
    /fall -> FALL;
    /reply -> ALERT_REPLY;
    /bloodoxygen -> BLOOD_OXYGEN;
    /exercise_heartrate -> EXERCISE_HEART_RATE;
    /notify -> NOTIFY;
  若路径也无法判断，按字段特征推断:
    含reply_type -> ALERT_REPLY;
    含sos -> SOS;
    含bloodoxygen -> BLOOD_OXYGEN;
    含dbp -> BLOOD_PRESSURE;
    含remaining_power -> POWER;
    含awake_time且同时含lon/lat/address -> FALL;
    含deep_sleep -> SLEEP;
    含step_number_value或stepNumber -> STEP;
    含max_heartrate -> EXERCISE_HEART_RATE;
    含heartrate时:
      如果同时有city/address/lon/lat且没有高低心率阈值 -> SOS;
      否则 -> HEART_RATE;
    含is_track -> LOCATION;
    含lon且含lat -> LOCATION;
    含deviceid且含type -> NOTIFY;
  最终都不命中则返回UNKNOWN;
}

公共方法 返回时间 名字是parseEventTime（入参：form）{
  如果time_begin_str非空:
    按"yyyyMMddHHmmss"解析，成功则返回;
  遍历time_begin、time_end、timestamp三个候选字段;
    对每个非空值依次尝试:
      RFC3339;
      "yyyy-MM-dd HH:mm:ss";
      "yyyy/MM/dd HH:mm:ss";
    解析成功则返回;
    若能转为整数:
      大于1,000,000,000,000按毫秒时间戳;
      大于0按秒时间戳;
      解析成功则返回;
  若全部失败返回零时间;
}

公共方法 返回时间 名字是chooseTimestamp（入参：eventTime）{
  如果eventTime是零值则返回当前时间;
  否则返回eventTime;
}

公共方法 返回字符串 名字是normalizePath（入参：path）{
  去除首尾空白;
  若为空返回"/";
  若不以"/"开头则补上;
  去掉末尾"/";
  若结果为空返回"/";
  返回规范化后的路径;
}

公共方法 返回字符串 名字是normalizeDataType（入参：name）{
  转大写并去空白;
  把"-"替换为"_";
  把空格替换为"_";
  若为空返回"UNKNOWN";
  返回处理结果;
}

公共方法 返回字符串 名字是clientIP（入参：req）{
  优先读请求头X-Forwarded-For，取第一个IP;
  否则读X-Real-IP;
  否则从RemoteAddr拆分host;
  都失败则返回RemoteAddr原始值;
}

公共方法 返回布尔 名字是ipAllowed（入参：ip、allowed）{
  ip去空白，为空直接返回false;
  循环allowed每一项:
    项去空白，空则跳过;
    若与ip完全相等返回true;
    若项包含"/"，按CIDR解析并判断ip是否落在网段内，命中返回true;
  循环结束仍未命中返回false;
}

---

公共初始化方法 名字是init（没有入参）{
  调用插件注册工具（插件名是"aiqiangua_x8"，工厂方法返回新的Worker实例）;
}

---

说明：
1) 此文档是 `worker.go` 的“流程/语义翻译版”，不是可直接编译的Go代码。
2) 表达方式按你给的示例风格：中文步骤化、函数式描述、强调入参与处理流程。



物模型 TSL
代码视图编辑器视图
{
  "productId": "2039993122955739138",
  "productKey": "dpdrwaz03sOVqScg",
  "properties": [
    {
      "identifier": "average_heartrate",
      "name": "平均心率",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 10,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "imei",
      "name": "设备序列号",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 255,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "max_heartrate",
      "name": "最大心率",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 10,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "min_heartrate",
      "name": "最小心率",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 10,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "step",
      "name": "运动步数",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 10,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "time_begin",
      "name": "发生日期",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 20,
        "defaultValue": null
      },
      "dataSpecsList": null
    }
  ],
  "events": [],
  "services": []
}



物模型 TSL
代码视图编辑器视图
{
  "productId": "2039992593043177474",
  "productKey": "Ijo8XeEESA7ztPbp",
  "properties": [
    {
      "identifier": "bloodoxygen",
      "name": "血氧百分百",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 10,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "imei",
      "name": "设备序列号",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 255,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "time_begin",
      "name": "发生时间",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 20,
        "defaultValue": null
      },
      "dataSpecsList": null
    }
  ],
  "events": [],
  "services": []
}


物模型 TSL
代码视图编辑器视图
{
  "productId": "2039991446559539202",
  "productKey": "2CLmawFciqXD4YO4",
  "properties": [
    {
      "identifier": "dbp",
      "name": "舒张压",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 10,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "imei",
      "name": "设备序列号",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 255,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "sbp",
      "name": "收缩压",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 10,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "time_begin",
      "name": "产生日期",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 20,
        "defaultValue": null
      },
      "dataSpecsList": null
    }
  ],
  "events": [],
  "services": []
}



物模型 TSL
代码视图编辑器视图
{
  "productId": "2039989653502967809",
  "productKey": "TBXhGV8LE44zgrAn",
  "properties": [
    {
      "identifier": "awake_time",
      "name": "清醒时长",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 20,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "deep_sleep",
      "name": "深睡时长",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 20,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "imei",
      "name": "设备序列号",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 255,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "light_sleep",
      "name": "浅睡时长",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 20,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "time_begin",
      "name": "开始时间",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 50,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "time_end",
      "name": "结束时间",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 50,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "total_sleep",
      "name": "总睡时长",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 20,
        "defaultValue": null
      },
      "dataSpecsList": null
    }
  ],
  "events": [],
  "services": []
}



物模型 TSL
代码视图编辑器视图
{
  "productId": "2039988498085462018",
  "productKey": "EGka3c3dThYN5j8d",
  "properties": [
    {
      "identifier": "imei",
      "name": "设备序列号",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 255,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "step_number_value",
      "name": "步数",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 10,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "time_begin",
      "name": "发生时间",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 50,
        "defaultValue": null
      },
      "dataSpecsList": null
    }
  ],
  "events": [],
  "services": []
}



物模型 TSL
代码视图编辑器视图
{
  "productId": "2039986795831377921",
  "productKey": "H1seuqXu1Zewtcvw",
  "properties": [
    {
      "identifier": "heartrate",
      "name": "心率",
      "accessMode": "rw",
      "required": null,
      "dataType": "int",
      "dataSpecs": {
        "dataType": "int",
        "max": "200",
        "min": "0",
        "step": null,
        "precise": null,
        "defaultValue": null,
        "unit": "count",
        "unitName": "次"
      },
      "dataSpecsList": null
    },
    {
      "identifier": "imei",
      "name": "设备序列号",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 255,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "time_begin",
      "name": "发生时间",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 50,
        "defaultValue": null
      },
      "dataSpecsList": null
    }
  ],
  "events": [],
  "services": []
}



物模型 TSL
代码视图编辑器视图
{
  "productId": "2039966012430106626",
  "productKey": "z1nl89moLybMXdvZ",
  "properties": [
    {
      "identifier": "address",
      "name": "详细地址",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 500,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "city",
      "name": "城市",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 255,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "imei",
      "name": "设备序列号",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 255,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "lat",
      "name": "纬度",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 255,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "lon",
      "name": "经度",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 255,
        "defaultValue": null
      },
      "dataSpecsList": null
    },
    {
      "identifier": "time_begin",
      "name": "发生时间",
      "accessMode": "rw",
      "required": null,
      "dataType": "text",
      "dataSpecs": {
        "dataType": "text",
        "length": 50,
        "defaultValue": null
      },
      "dataSpecsList": null
    }
  ],
  "events": [],
  "services": []
}

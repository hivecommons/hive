package customers

// The bundled place table behind DeriveCountry. It is data, not logic: every
// name is lower case with accents folded (see foldPlace), one country per
// line as "ISO|English name|alias;alias". No network, no geocoder.
const countryTable = `AD|andorra|
AE|united arab emirates|uae;emirates;dubai;abu dhabi
AF|afghanistan|
AG|antigua and barbuda|antigua
AI|anguilla|
AL|albania|
AM|armenia|
AO|angola|
AR|argentina|
AS|american samoa|
AT|austria|osterreich
AU|australia|
AW|aruba|
AX|aland islands|
AZ|azerbaijan|
BA|bosnia and herzegovina|bosnia
BB|barbados|
BD|bangladesh|
BE|belgium|belgique;belgie
BF|burkina faso|
BG|bulgaria|
BH|bahrain|
BI|burundi|
BJ|benin|
BL|saint barthelemy|
BM|bermuda|
BN|brunei|brunei darussalam
BO|bolivia|
BQ|bonaire sint eustatius and saba|
BR|brazil|brasil
BS|bahamas|
BT|bhutan|
BW|botswana|
BY|belarus|
BZ|belize|
CA|canada|
CC|cocos islands|
CD|democratic republic of the congo|dr congo;drc
CF|central african republic|
CG|republic of the congo|congo
CH|switzerland|schweiz;suisse;svizzera
CI|cote d ivoire|ivory coast
CK|cook islands|
CL|chile|
CM|cameroon|
CN|china|prc;peoples republic of china;mainland china
CO|colombia|
CR|costa rica|
CU|cuba|
CV|cabo verde|cape verde
CW|curacao|
CX|christmas island|
CY|cyprus|
CZ|czechia|czech republic
DE|germany|deutschland
DJ|djibouti|
DK|denmark|danmark
DM|dominica|
DO|dominican republic|
DZ|algeria|
EC|ecuador|
EE|estonia|
EG|egypt|
EH|western sahara|
ER|eritrea|
ES|spain|espana
ET|ethiopia|
FI|finland|suomi
FJ|fiji|
FK|falkland islands|
FM|micronesia|
FO|faroe islands|
FR|france|
GA|gabon|
GB|united kingdom|uk;great britain;britain;england;scotland;wales;northern ireland;united kingdom of great britain and northern ireland
GD|grenada|
GE|republic of georgia|
GF|french guiana|
GG|guernsey|
GH|ghana|
GI|gibraltar|
GL|greenland|
GM|gambia|
GN|guinea|
GP|guadeloupe|
GQ|equatorial guinea|
GR|greece|
GT|guatemala|
GU|guam|
GW|guinea bissau|
GY|guyana|
HK|hong kong|
HN|honduras|
HR|croatia|hrvatska
HT|haiti|
HU|hungary|
ID|indonesia|
IE|ireland|republic of ireland
IL|israel|
IM|isle of man|
IN|india|bharat
IO|british indian ocean territory|
IQ|iraq|
IR|iran|
IS|iceland|
IT|italy|italia
JE|jersey|
JM|jamaica|
JO|jordan|
JP|japan|
KE|kenya|
KG|kyrgyzstan|
KH|cambodia|
KI|kiribati|
KM|comoros|
KN|saint kitts and nevis|
KP|north korea|
KR|south korea|korea;republic of korea
KW|kuwait|
KY|cayman islands|
KZ|kazakhstan|
LA|laos|
LB|lebanon|
LC|saint lucia|
LI|liechtenstein|
LK|sri lanka|
LR|liberia|
LS|lesotho|
LT|lithuania|
LU|luxembourg|
LV|latvia|
LY|libya|
MA|morocco|
MC|monaco|
MD|moldova|
ME|montenegro|
MF|saint martin|
MG|madagascar|
MH|marshall islands|
MK|north macedonia|macedonia
ML|mali|
MM|myanmar|burma
MN|mongolia|
MO|macao|macau
MP|northern mariana islands|
MQ|martinique|
MR|mauritania|
MS|montserrat|
MT|malta|
MU|mauritius|
MV|maldives|
MW|malawi|
MX|mexico|mexico
MY|malaysia|
MZ|mozambique|
NA|namibia|
NC|new caledonia|
NE|niger|
NF|norfolk island|
NG|nigeria|
NI|nicaragua|
NL|netherlands|holland;nederland
NO|norway|norge
NP|nepal|
NR|nauru|
NU|niue|
NZ|new zealand|aotearoa
OM|oman|
PA|panama|
PE|peru|
PF|french polynesia|
PG|papua new guinea|
PH|philippines|
PK|pakistan|
PL|poland|polska
PM|saint pierre and miquelon|
PN|pitcairn islands|
PR|puerto rico|
PS|palestine|
PT|portugal|
PW|palau|
PY|paraguay|
QA|qatar|
RE|reunion|
RO|romania|
RS|serbia|
RU|russia|russian federation
RW|rwanda|
SA|saudi arabia|ksa
SB|solomon islands|
SC|seychelles|
SD|sudan|
SE|sweden|sverige
SG|singapore|
SH|saint helena|
SI|slovenia|
SJ|svalbard and jan mayen|
SK|slovakia|slovak republic
SL|sierra leone|
SM|san marino|
SN|senegal|
SO|somalia|
SR|suriname|
SS|south sudan|
ST|sao tome and principe|
SV|el salvador|
SX|sint maarten|
SY|syria|
SZ|eswatini|swaziland
TC|turks and caicos islands|
TD|chad|
TG|togo|
TH|thailand|
TJ|tajikistan|
TK|tokelau|
TL|timor leste|east timor
TM|turkmenistan|
TN|tunisia|
TO|tonga|
TR|turkey|turkiye
TT|trinidad and tobago|trinidad
TV|tuvalu|
TW|taiwan|
TZ|tanzania|
UA|ukraine|
UG|uganda|
US|united states|usa;us;america;united states of america
UY|uruguay|
UZ|uzbekistan|
VA|vatican city|holy see
VC|saint vincent and the grenadines|
VE|venezuela|
VG|british virgin islands|
VI|us virgin islands|
VN|vietnam|viet nam
VU|vanuatu|
WF|wallis and futuna|
WS|samoa|
XK|kosovo|
YE|yemen|
YT|mayotte|
ZA|south africa|
ZM|zambia|
ZW|zimbabwe|`

// subdivisionTable maps the states, provinces and regions a location often
// ends with to their country: "ISO|name;name". Two-letter abbreviations are
// listed apart (stateAbbrevs) because they collide with ISO codes.
const subdivisionTable = `US|alabama;alaska;arizona;arkansas;california;colorado;connecticut;delaware;florida;georgia;hawaii;idaho;illinois;indiana;iowa;kansas;kentucky;louisiana;maine;maryland;massachusetts;michigan;minnesota;mississippi;missouri;montana;nebraska;nevada;new hampshire;new jersey;new mexico;new york;north carolina;north dakota;ohio;oklahoma;oregon;pennsylvania;rhode island;south carolina;south dakota;tennessee;texas;utah;vermont;virginia;washington dc;washington d c;west virginia;washington;wisconsin;wyoming;district of columbia
CA|alberta;british columbia;manitoba;new brunswick;newfoundland and labrador;nova scotia;ontario;prince edward island;quebec;saskatchewan;yukon;northwest territories;nunavut
AU|new south wales;queensland;south australia;tasmania;victoria australia;western australia;northern territory;australian capital territory
IN|maharashtra;karnataka;tamil nadu;kerala;telangana;gujarat;rajasthan;uttar pradesh;west bengal;haryana;punjab india;andhra pradesh
DE|bavaria;bayern;baden wurttemberg;north rhine westphalia;nordrhein westfalen;hessen;saxony;niedersachsen;lower saxony;brandenburg
BR|sao paulo state;rio grande do sul;minas gerais;santa catarina;parana`

// stateAbbrevs are the two-letter postal codes of US states, and a few
// unambiguous Canadian and Australian ones, as they appear upper-case after a
// comma ("Austin, TX").
const stateAbbrevs = `US|AL AK AZ AR CA CO CT DE DC FL GA HI ID IL IN IA KS KY LA ME MD MA MI MN MS MO MT NE NV NH NJ NM NY NC ND OH OK OR PA RI SC SD TN TX UT VT VA WA WV WI WY
CA|ON BC QC AB
AU|NSW QLD VIC`

// cityTable lists major cities that name their country on their own:
// "ISO|city;city". A city that is also a US place (Paris, Texas) is overruled
// by an explicit state or country after it.
const cityTable = `US|new york city;nyc;los angeles;san francisco;sf bay area;bay area;silicon valley;san jose;san diego;seattle;boston;chicago;austin;houston;dallas;denver;atlanta;miami;portland;philadelphia;phoenix;san antonio;las vegas;pittsburgh;raleigh;durham;minneapolis;detroit;salt lake city;nashville;brooklyn;manhattan;cambridge ma;palo alto;mountain view;sunnyvale;redmond;cupertino;menlo park;boulder;baltimore;columbus;charlotte;st louis;kansas city;orlando;tampa;honolulu;anchorage
GB|london;manchester;birmingham;edinburgh;glasgow;bristol;leeds;liverpool;cardiff;belfast;oxford;cambridge uk;sheffield;newcastle;nottingham
CA|toronto;vancouver;montreal;ottawa;calgary;edmonton;winnipeg;halifax;waterloo
DE|berlin;munich;hamburg;frankfurt;cologne;stuttgart;dusseldorf;dresden;leipzig;nuremberg;karlsruhe;bonn;heidelberg
FR|paris;lyon;marseille;toulouse;nice;nantes;bordeaux;lille;strasbourg;grenoble;montpellier
ES|madrid;barcelona;valencia;seville;bilbao;malaga;zaragoza
IT|rome;milan;turin;naples;florence;bologna;venice
NL|amsterdam;rotterdam;utrecht;eindhoven;hague;den haag;delft
BE|brussels;antwerp;ghent;leuven
CH|zurich;geneva;basel;bern;lausanne
AT|vienna;graz;salzburg;linz
SE|stockholm;gothenburg;malmo;uppsala
NO|oslo;bergen;trondheim
DK|copenhagen;aarhus;odense
FI|helsinki;espoo;tampere;turku
IE|dublin;cork;galway
PT|lisbon;porto;braga
PL|warsaw;krakow;wroclaw;gdansk;poznan
CZ|prague;brno
HU|budapest
RO|bucharest;cluj napoca;cluj
GR|athens;thessaloniki
UA|kyiv;kiev;lviv;kharkiv;odesa
RU|moscow;saint petersburg;st petersburg;novosibirsk
TR|istanbul;ankara;izmir
IL|tel aviv;jerusalem;haifa
AE|dubai;abu dhabi
SA|riyadh;jeddah
EG|cairo;alexandria
ZA|cape town;johannesburg;pretoria;durban
NG|lagos;abuja
KE|nairobi
GH|accra
MA|casablanca;rabat
IN|bangalore;bengaluru;mumbai;delhi;new delhi;hyderabad;chennai;pune;kolkata;ahmedabad;gurgaon;gurugram;noida
PK|karachi;lahore;islamabad
BD|dhaka
LK|colombo
CN|beijing;shanghai;shenzhen;guangzhou;hangzhou;chengdu;wuhan;nanjing;xian
HK|hong kong
TW|taipei;hsinchu;kaohsiung
JP|tokyo;osaka;kyoto;yokohama;nagoya;fukuoka;sapporo
KR|seoul;busan;incheon
SG|singapore
MY|kuala lumpur;penang
TH|bangkok;chiang mai
VN|hanoi;ho chi minh city;saigon;da nang
ID|jakarta;bandung;bali;surabaya
PH|manila;cebu;quezon city
AU|sydney;melbourne;brisbane;perth;adelaide;canberra
NZ|auckland;wellington;christchurch
BR|sao paulo;rio de janeiro;brasilia;belo horizonte;curitiba;porto alegre;recife;florianopolis
AR|buenos aires;cordoba;rosario
CL|santiago
CO|bogota;medellin
PE|lima
MX|mexico city;guadalajara;monterrey;tijuana
UY|montevideo
CR|san jose costa rica
GE|tbilisi`
